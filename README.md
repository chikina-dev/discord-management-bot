# Discord Management Bot

Discordサーバー「times_ちきな(裏)」のGatewayイベントを受け取り、SQLiteに保存したトリガーで照合して各Cloudflare Workerへ配信する。
EC2は1つの受信用Botで接続する。Discordへの投稿は、各Workerが自分のBot Tokenで行う。

```text
Discord Gateway → EC2：保存・照合・配信 → Worker：受付・Botの処理
                  ↑                    │
                  └── 署名付きの結果通知 ┘
PC ── WireGuard ── 管理API
```

## 設計

| 担当 | 責任 |
| --- | --- |
| Terraform | AWSリソースとSSMパラメーターの管理、EC2の署名鍵・WireGuard秘密鍵の生成と保持 |
| Ansible | EC2のOS・EBSマウント・WireGuard・systemd・監視・バイナリ配置 |
| Go | Gateway接続・条件式・配信・結果通知・管理API・SQLite・バックアップ・起動時のDNS更新 |
| Worker | 重複排除、各Botの処理、Discord投稿、最終結果の通知 |
| Skill | 接続済みのWireGuardを通じた管理API操作。未接続なら起動を依頼 |

SSMは[parameters.tf](infra/parameters.tf)で作成・更新する。
入力は既存BotのToken・対象サーバーID・通知Webhook URL・PCのWireGuard公開鍵の4つ。
EC2用の署名seedとWireGuard秘密鍵はTerraformが生成する。SSMの手動登録や、公開鍵のソースへの埋め込みは不要。
[設計](docs/design.md)に依存関係と鍵の所有者を記載する。

2026-11-13に承認済みplanで実環境へデプロイした。[検証結果](docs/deployment.md)を参照。

## ローカル検証

```sh
go test -race ./...
terraform -chdir=infra test
ANSIBLE_CONFIG=ansible/ansible.cfg ansible-playbook -i test/ansible/inventory.yml ansible/site.yml --syntax-check
ANSIBLE_CONFIG=ansible/ansible.cfg ansible-playbook -i test/ansible/inventory.yml test/ansible/wireguard.yml
```

TerraformテストはAWSとrandomをモックする。
ホストの変更別起動はtest/ansible/activation.yml、所有権移行は使い捨てLinuxコンテナ内のtest/ansible/storage_identity.ymlで検証する。実機systemdの代わりにテスト用adapterを使う。
WireGuardのテストは一時ファイルへの描画と鍵形式の検証を行い、VPNを起動しない。
GoのテストにはGo 1.26以降とCコンパイラー、署名互換性テストにはNode.js、Ansibleのローカル検証にはAnsibleとWireGuard toolsを使う。

## 承認後の構築・更新

AWSは`default`プロファイル、アカウント`435684131663`、東京リージョン。
`t4g.nano`、専用8GiB EBS、自動割当の公開IPv4を使う。公開SSHは使わない。
操作PCにはTerraform・Docker・AWS CLIを用意する。実機用AnsibleはLinuxコンテナに含める。

1. 初回は`infra/bootstrap`でstate・Ansible一時転送用S3を構築する。
2. リポジトリ直下の`.env`に次の4項目を指定する。[.env.example](.env.example)を参照。

   ```dotenv
   DISCORD_BOT_TOKEN="..."
   DISCORD_SERVER_ID="..."
   DISCORD_WEBHOOK_URL="..."
   PC_WIREGUARD_PUBLIC_KEY="..."
   ```

   Terraformがこのファイルを直接読み、SSMを管理する。tfvarsへの転記やshellからのsourceは不要。
3. Terraformのplanをレビューし、承認された差分だけ適用する。SSMと生成鍵もこの段階で管理する。
4. Linux ARM64バイナリをビルドし、AnsibleでEC2を構成して起動する。

既存SSMと鍵を引き継ぐ場合は、先に[引継ぎ方針](docs/terraform-adoption.md)を確認する。
SSMだけをimportして生成鍵を新規作成すると鍵が変わるため、両方を同じ値で取り込む。
秘密値を含む.env・plan・stateはGitや共有出力へ含めない。stateは暗号化した非公開S3で保持する。

```sh
terraform -chdir=infra init -backend-config=backend.hcl
terraform -chdir=infra plan -out=review.tfplan
```

適用の承認後に、保存したplanを使う。

```sh
terraform -chdir=infra apply review.tfplan
docker build --platform linux/arm64 -f Dockerfile.build --target artifact --output type=local,dest=dist .
docker build --platform linux/arm64 -f ansible/Dockerfile -t discord-gateway-ansible .
docker run --rm --platform linux/arm64 \
  --mount type=bind,src="$PWD",dst=/workspace \
  --mount type=bind,src="$HOME/.aws",dst=/root/.aws,readonly \
  discord-gateway-ansible site.yml
```

## WireGuardと管理API

AnsibleがTerraform管理のSSM値からEC2の`wg0.conf`を配置し、`wg-quick@wg0`を起動する。
EC2の公開鍵を取得した後、PC用の`.local/wireguard-client.conf.example`を出力する。
PCの秘密鍵は含めない。
`[Interface]`の`PrivateKey`には、`.env`の`PC_WIREGUARD_PUBLIC_KEY`に対応する既存のPC秘密鍵を指定する。
`[Peer]`の`PublicKey`はEC2の公開鍵であり、Ansibleが設定例へ出力する。
PC用の鍵ペアをこのシステムで生成する処理はない。

| 用途 | アドレス |
| --- | --- |
| EC2のWireGuard | `10.203.0.1`、UDP 51820 |
| PCのWireGuard | `10.203.0.2` |
| 管理API | `http://10.203.0.1:8080` |
| Workerの結果通知 | `https://discord-gateway.chigayuki.com/results` |

PC側は`10.203.0.1/32`だけをトンネルへ通す。
管理は配信先を作り、そのIDを参照するトリガーを作る。

```sh
curl --fail-with-body http://10.203.0.1:8080/status
curl --fail-with-body http://10.203.0.1:8080/signing-key
```

[管理API](docs/management-api.md)・[条件式](docs/conditions.md)・[Workerとの通信仕様](docs/protocol.md)を参照。
トリガー削除で新規照合と未受付の再送を止め、履歴は保持する。
URL・公開鍵の変更は未送信分に適用し、送信済み分は初回送信時のURL・鍵で完了させる。

## 永続化と監視

DBは専用EBSの`/var/lib/discord-gateway/gateway.db`。
配信履歴・結果とCloudWatchログは1年、毎時のSQLiteバックアップはS3で30日保持する。
基盤異常・復旧はCloudWatch→EventBridge→Discord Webhookで通知する。
Workerの失敗・一部成功は記録と可視化の対象とする。
DB復旧はGateway・timer・実行中backupの停止と共通の排他lockを必要とする。
EC2交換時はAnsibleがEBS内の数値ownerを整合する。再起動・復旧は[運用手順](docs/operations.md)を参照。

実際の`t4g.nano`で待受時のメモリ使用量を測定済み。
PDFプレビューは新Gateway経由のsuccess結果を確認済み。
イベント配信時の負荷測定とTimelineの移行は未完了。
