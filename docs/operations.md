# 運用手順

Ansibleの操作は実行用コンテナ内の/workspace/ansibleから行う。
リポジトリ直下で実行用コンテナをビルドする。

```sh
docker build --platform linux/arm64 -f ansible/Dockerfile -t discord-gateway-ansible .
```

対話操作には以下を使う。

```sh
docker run --rm -it --platform linux/arm64 --entrypoint /bin/bash \
  --mount type=bind,src="$PWD",dst=/workspace \
  --mount type=bind,src="$HOME/.aws",dst=/root/.aws,readonly \
  discord-gateway-ansible
```

対象EC2はNameタグがdiscord-management-botの稼働中インスタンス1台を自動で取得する。
PCのWireGuard接続前でもSSMでホストを管理できる。

## 再配置・状態確認

Goの変更後はリポジトリ直下でARM64バイナリをビルドし、実行用コンテナ内のansible-playbook site.ymlで配置する。

```sh
docker build --platform linux/arm64 -f Dockerfile.build --target artifact --output type=local,dest=dist .
```

Gatewayのbinary・unit、または参照するSSMパラメーターのVersionが変わった場合にGatewayを再起動する。
変更判定には受信用Bot Token・サーバーID・署名seedのSSM Versionを使う。
Goは値を起動時にSSMから取得する。管理listenerのhealth確認後に適用済みVersionを記録する。
backup unitの変更では定義だけをreloadし、timer変更ではtimerを再起動する。DNS unit・共用binaryの変更はDNS updaterも再実行する。
変更のない再配置で、Gatewayを再起動しない。

```sh
ansible aws_ec2 -b -m ansible.builtin.command -a 'systemctl status discord-gateway wg-quick@wg0 amazon-cloudwatch-agent --no-pager'
ansible aws_ec2 -b -m ansible.builtin.command -a 'wg show wg0'
ansible aws_ec2 -b -m ansible.builtin.command -a 'tail -n 30 /var/log/discord-gateway/gateway.log'
```

Gatewayを明示的に再起動する場合は以下を使う。

```sh
ansible aws_ec2 -b -m ansible.builtin.systemd_service -a 'name=discord-gateway state=restarted'
```

Bot Token・サーバーID・署名seedが欠けている場合は、Gatewayは起動時にエラーを返す。
設定変更はリポジトリ直下の.envからTerraformでSSMへ反映し、Ansibleで再配置する。

## WireGuardと接続先の更新

AnsibleはEC2のWireGuard設定を配置し、PC用の設定例を.local/wireguard-client.conf.exampleへ出力する。
PCのPrivateKeyには、.envのPC_WIREGUARD_PUBLIC_KEYに対応する秘密鍵を指定する。

EC2を停止・起動して公開IPv4が変わった場合は、discord-gateway-dns.serviceが自身のAレコードを更新する。
DNS updaterは独立したoneshotであり、失敗後の再試行はsystemdが行う。DNSの一時失敗でイベント受信全体を起動不能にしない。
WireGuardの初回起動に失敗して管理listenerをbindできない場合も、Gatewayのsystemd retryで回復する。
WireGuardのEndpointと結果通知URLは同じDNS名を使う。
PC側で古い接続先が保持されている場合は、利用者がトンネルを接続し直す。

## 監視

[CloudWatchダッシュボード](https://ap-northeast-1.console.aws.amazon.com/cloudwatch/home?region=ap-northeast-1#dashboards/dashboard/discord-management-bot)で、接続状態・配信滞留・Workerの結果・バックアップ経過時間・ホストの使用量を確認する。
ロググループは/discord-management-bot/gateway。
基盤異常と復旧はDiscord Webhookへ通知し、Workerの失敗・一部成功は記録と可視化で扱う。

## バックアップと復旧

- DB・Resume情報・配信先・トリガー・履歴は専用EBSの/var/lib/discord-gatewayに保存する。
- discord-gateway-backup.timerが毎時バックアップを実行する。
- 保存先はs3://discord-gateway-backup-435684131663-ap-northeast-1/sqlite/。
- SQLiteのオンラインバックアップで整合したスナップショットを作り、gzip圧縮してからS3へ送る。S3保存成功後にだけ最終成功時刻を記録する。

EC2だけを作り直す場合は、同じ専用EBSを接続してAnsibleを実行する。
Ansibleは既存ファイルの数値UID/GIDを検査し、不一致時だけtimer・Gateway・実行中backupを停止する。
同じmaintenance lockを排他で保持して所有者だけを整合し、既存の0600・0700とlock inodeを保つ。

バックアップから戻す場合は、次の順序で作業する。

1. 操作PCのAWS権限で選んだS3バックアップを取得して展開する。SQLiteのintegrity_checkがokであること、旧DB一式の退避と置換DBを保持する容量があることを確認する。足りなければ置換を始めない。
2. 復旧対象EC2へ候補DBを転送する。実行中アプリのgateway.dbへ直接上書きしない。
3. Gateway、バックアップtimer、実行中のバックアップserviceをすべて停止し、停止完了を待つ。timerだけを止めても実行中backupは終了しない。
4. SSMの管理sessionでrootになり、下記の排他lock付きshellへ入る。以降のDB置換と検証はこのshell内で行う。
5. 現在のDB・WAL・SHMを一式で退避する。lockファイルは退避・削除・置換しない。
6. 候補を同じfilesystem内の一時名へ配置し、integrity_checkを再確認してからgateway.dbへrenameする。旧WAL・SHMを新DBと組み合わせない。
7. DBとmaintenance lockの所有者をdiscord-gateway、modeを0600に揃える。既存の証明書などへ再帰的なchmodは行わない。
8. shellをexitして排他lockを解放する。その後Gatewayを起動し、管理APIのhealthを確認してからtimerを起動する。
9. /statusとCloudWatchでGateway接続・未受付配信・バックアップを確認する。

停止とlock取得にはsystemctl・flockを使う。

```sh
systemctl stop discord-gateway-backup.timer discord-gateway.service discord-gateway-backup.service
flock --exclusive /var/lib/discord-gateway/.maintenance.lock /bin/bash
```

Gatewayとbackupは、同じファイルの共有lockをwriterの寿命中保持する。
復旧の排他lock中に新しいwriterが起動しても、DBを開く前に失敗する。
排他lockの取得に失敗した場合はwriterを調べ、lockファイルの削除で迂回しない。

バックアップ処理は欠損・空のDBを初期化せず、既存のgateway schemaを要求する。
snapshotはscratch専用lock内で1つだけ作り、1GiBの空きを残すサイズ上限をnative copyへ適用する。
gzip出力は5MiBずつS3 multipartへ送るため、圧縮済みの2つ目の一時ファイルは作らない。
失敗・キャンセル時はuploadをabortして清掃し、S3完了後にだけ成功時刻を更新する。
OOMなどで残ったsnapshotは、次のbackupがscratch lockを取得してから清掃する。

Discord側のResumeが成立しない区間について、履歴APIからの補完配信は行わない。

## パッケージと一時転送

Ansibleは未導入パッケージがある場合にだけ512MiBの一時ディスクswapを使う。
導入後はswapを解除してファイルも削除する。

[AnsibleのSSM接続](https://docs.ansible.com/projects/ansible/latest/collections/amazon/aws/aws_ssm_connection.html)は一時ファイル転送にS3を使う。
専用転送バケットは非公開・暗号化し、バージョニングを有効にしない。
転送ファイルはAnsibleが削除し、中断して残ったファイルは1日で失効する。

コンテナにはソースや秘密情報を焼き込まず、AWS認証情報は実行時に読み取り専用で渡す。
