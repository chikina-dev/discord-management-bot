# 設計

対象はDiscordサーバー「times_ちきな(裏)」1つ。
EC2の受信用BotでGatewayイベントを受信し、保存した条件式に従って各WorkerへHTTP配信する。
各Workerは自分のBot TokenでDiscordへ投稿する。

## 責任の所在

| 担当 | 所有するもの |
| --- | --- |
| Terraform | AWSリソース、SSMパラメーター、EC2用の署名鍵・WireGuard秘密鍵の生成と保持 |
| Ansible | OSパッケージ、専用EBSのマウント、WireGuard、systemd、監視設定、Goバイナリの配置 |
| Go | Gateway接続、受信データの永続化、トリガー照合、HTTP配信、結果記録、管理API、SQLiteバックアップ、起動時のDNS更新 |
| Worker | 配信の署名検証と重複排除、各Botの処理、Discord投稿、最終結果の通知 |
| 管理Skill | 接続済みのWireGuardを通じた管理API操作 |

リポジトリ直下の.envをTerraformが直接読む。
入力名はDISCORD_BOT_TOKEN・DISCORD_SERVER_ID・DISCORD_WEBHOOK_URL・PC_WIREGUARD_PUBLIC_KEY。
これらを受信用Bot Token・サーバーID・通知Webhook URL・PCのWireGuard公開鍵としてSSMへ保存する。
tfvarsとの二重管理や、shellによる変数展開・実行は行わない。
EC2の署名seedとWireGuard秘密鍵はTerraformのrandom_bytesで生成し、stateに保持する。
SSMの手動作成・手動登録、AnsibleやGoからのSSM書込みは行わない。

固定の構成値は、その構成を組み立てるコードに置く。
アプリの保存先・待受ポート・ホスト名を引数やenvの選択肢に展開しない。
HTTPクライアント、DB、鍵などの実際の依存先はテストコードから渡せるようにする。
本番コードにローカルモードや模擬Gateway用の分岐を持たせない。

## 構築の依存関係

1. Terraformのbootstrapで、stateとAnsible一時転送のS3を用意する。
2. Terraformで入力値・生成鍵のSSMパラメーターと、EC2・EBS・ネットワーク・IAM・DNS・監視・通知経路を管理する。
3. Linux ARM64バイナリをビルドする。
4. AnsibleがSSM経由でEC2に接続し、マウント・WireGuard・監視・systemd・アプリを構成して起動する。
5. AnsibleがEC2のWireGuard公開鍵を取得し、PCの秘密鍵を含まないクライアントconfigをローカルへ出力する。

構築と起動を、秘密情報の未登録を理由に分割するフラグは設けない。
入力とSSMはTerraformで完成させてからAnsibleへ進む。
適用・起動・importなどの実環境操作は、レビュー後に明示された範囲でのみ実行する。

## SSMと鍵の管理

SSMパラメーターは[parameters.tf](../infra/parameters.tf)がすべて宣言する。

| 名前 | 種類 | 値の所有者 | 利用先 |
| --- | --- | --- | --- |
| bot-token | SecureString | 既存のDiscord受信用Bot | Go |
| guild-id | String | 対象のDiscordサーバー | Go |
| signing-seed | SecureString | Terraformが生成・保持 | Go |
| wireguard-server-key | SecureString | Terraformが生成・保持 | Ansible |
| wireguard-client-public-key | String | PCの既存WireGuard鍵 | Ansible |
| discord-webhook-url | SecureString | 既存のDiscord通知Webhook | TerraformのAPI Destination |

接頭辞は/discord-management-bot。
GoのIAMロールに許可するSSM参照先はbot-token・guild-id・signing-seedの3つだけ。
Terraformの生成鍵は、EC2の再起動・再配置・再作成を理由に作り直さない。
既存の手動作成済みパラメーターと鍵は、値を維持してTerraformへ引き継ぐ。[引継ぎ方針](terraform-adoption.md)を参照。

## イベント処理とトリガー

Discordから受け取ったdataを変更せず、GatewayのSession・Sequenceと同じトランザクションで永続化する。
トリガーは条件式と1つの配信先を持つ。配信先は複数のトリガーから参照できる。
同じイベントが複数のトリガーに一致した場合は、それぞれに配信する。

条件式は字句解析・構文解析して構文木を作り、登録・更新・起動時に評価器へ変換する。
AND／ORや比較、配列判定の仕様は[条件式](conditions.md)に定める。
添付に関係する条件では、一致した元の添付オブジェクトをルートのattachmentsへ追加する。
元のdata.attachmentsは変更しない。
チャンネル情報などの補助情報はcontextに置く。

カタログ変更は、次の評価器snapshotを保存前に準備する。
保存後の値はDBのtransaction内で読み、commit後はI/Oなしでsnapshotを公開する。
HTTPリクエストの取消や読取り失敗で、保存済み設定と稼働中評価器を分裂させない。

Discordへのメタデータ取得中に、トリガー管理のロックを保持しない。
取得中に設定が変わった場合は最新設定で再評価し、削除済み・変更前のトリガーで配信を作らない。
contextは最初に正常にcaptureできた時点のsnapshotをDBに保存し、条件評価の再試行で同じsnapshotを使う。受信時点の履歴を遡って再構成するものではない。
不足したチャンネル情報を後から補完する場合も、チャンネル観測の反映は繰り返さない。
受信順序を永続化し、チャンネルごとの反映済み順序より古い観測でcacheを巻き戻さない。
イベントを処理済みにするとcontextと受信順序の行も外部キーで削除する。
配信先を削除すると、関連トリガーも外部キーで削除する。
一時停止フラグは設けず、削除で新規照合と未受付の再送を止める。
過去の配信履歴と、送信済み分の結果通知の対応付けは保持する。

## 配信と結果通知

配信ごとにdelivery_idを作り、再送でもIDと本文を変えない。
Workerの202応答を受付として記録し、Workerは処理終了後に署名付きの結果通知を送る。
最終結果はsuccess・partial_success・failure・skippedの4種類。配信1件を単位とする。

EC2受信・EC2初回送信・Worker受付・Worker処理終了・EC2結果受信の5時点を記録する。
署名の鮮度確認用時刻はこれらとは別に持ち、再送ごとに更新する。許容幅は両方向とも5分。
詳細は[通信仕様](protocol.md)に定める。スキーマのversionフィールドは設けない。

未受付の通信失敗はバックオフ付きで再送する。最大間隔は5分。
Worker側の重複排除が、同じ配信の処理の重複を防ぐ。
URLと結果検証用公開鍵も初回送信時に固定し、配信先の更新は未送信分に適用する。
送信済み分は旧URL・旧公開鍵のまま処理を完了する。鍵更新は即時失効ではない。
結果の保存は署名検証済み鍵IDを条件に含めて原子的に行い、事前の配信snapshot取得から保存までの競合で認可をすり抜けない。
PDF投稿から10分といった業務上の期限は、WorkerがDiscordのtimestampから判断する。
再接続はResumeと保存済みデータの再送までとし、Discordの履歴APIから補完しない。

## ネットワークと運用

AWSはdefaultプロファイル、435684131663、ap-northeast-1。
EC2はap-northeast-1aのt4g.nano、自動割当の公開IPv4を使う。
固定IPv4・IPv6・公開SSHは使わない。
公開するポートはTCP 80・443とUDP 51820。

管理APIはWireGuard側の10.203.0.1:8080に待ち受ける。
PCは10.203.0.2を使い、10.203.0.1/32だけをトンネルへ通す。
公開HTTPSはWorkerの結果通知を受け、管理APIを公開しない。
公開IPv4の変更は起動時のDNS更新でdiscord-gateway.chigayuki.comへ反映する。
PCのWireGuard接続がない場合、Skillは「WireGuardを起動してください」と依頼する。

SQLiteは専用EBSに保存する。
配信履歴・結果とCloudWatchログは1年、毎時の整合したS3バックアップは30日保持する。
未完了の配信を経過日数だけで削除しない。
基盤異常・復旧はCloudWatch→EventBridge→API Destination→Discord Webhookで通知する。
Workerの失敗・一部成功は記録と可視化の対象とし、Discordの基盤異常通知には流さない。

PDFプレビュー側の変更・移行は別作業。Timelineの移行はその後に進める。
性能は実際のt4g.nanoで、WireGuard・監視・SSM Agentも含む通常時・負荷時・バックアップ時を測定する。

## 永続データの保守とホストの起動

Gatewayとバックアップは、同じmaintenance lockを共有で保持する。
復旧・所有権移行は、Gateway・バックアップtimer・実行中backupを停止してから同じlockを排他で保持する。
lockファイルを削除・置換しないため、途中で別のwriterが起動してもDB置換へ入り込めない。

バックアップは既存DBだけを開き、欠損時に空DBを作らない。
snapshotは元DBと同じEBS上に1つだけ作り、1GiBの空きを残す上限をnative copyにも適用する。
gzip出力は5MiBずつS3 multipartへ送り、圧縮済みの2つ目の一時ファイルを作らない。
同時バックアップはscratch側の別lockで排他にし、中断後のsnapshot残骸もそのlock内で清掃する。
失敗・キャンセルではuploadをabortし、S3完了後にだけ成功時刻を記録する。

AnsibleはEBS内の数値UID/GIDを検査し、不一致時だけwriterを静止させて所有者を整合する。
再帰的なmode変更は行わず、DBや証明書の0600・0700を保持する。
起動操作は変更された対象ごとに決め、backup-onlyのunit変更でGatewayを再起動しない。
Goの参照するSSMパラメーターはVersionだけを比較し、変更時にGatewayを再起動する。秘密値はホストの設定ファイルへ保存しない。
適用済みVersionは管理listenerのhealth確認後に記録する。

DNS updaterは独立し、失敗後のprocess retryはsystemdが所有する。
GatewayをDNSの成功に従属させず、WireGuardの初回失敗もlistenerのbind失敗によるprocess retryで回復する。
Ansibleは配置時のreadinessを確認し、常駐retry controllerにはならない。
