# 実環境のデプロイ記録

2026-11-13、対象サーバーがtimes_ちきな(裏)であることを確認し、利用者が承認した保存済みplanを適用した。
TerraformによるEC2の交換・リソース削除はない。
既存の署名鍵とWireGuard秘密鍵を引き継ぎ、.envの入力4項目をTerraform管理のSSMへ反映した。

| 項目 | 確認結果 |
| --- | --- |
| EC2 | i-0f657220df62c47a0、t4g.nano、稼働中 |
| Gateway | Discord認証とHeartbeat ACKを確認 |
| 管理API | EC2内からhttp://10.203.0.1:8080/statusを確認 |
| 公開HTTPS | TLS検証成功。GET /とGET /statusは404、署名なしPOST /resultsは401 |
| WireGuard | EC2側wg0が起動済み |
| バックアップ | systemdの初回実行成功。S3保存物を元のEC2内で展開し、SQLiteのintegrity_checkがok、外部キー違反0件を確認。検証用コピーは削除済み |
| CloudWatch | AppHeartbeat・GatewayConnected・BackupAgeSeconds・ホストメモリの実データを確認。基盤アラーム7件がOK |
| EventBridge | ConnectionがAUTHORIZED、API DestinationがACTIVE、異常・復旧の両ルールがENABLED |
| 再配置 | 同一構成でAnsibleを再実行し、変更0件・失敗0件 |
| ローカルテスト | go test -race ./...が成功 |

デプロイ時に、Discordが返すsession_start_limit.reset_after=0を誤って拒否する処理を修正した。
S3 multipart uploadの開始・各part・完了でCRC32の指定を揃え、初回バックアップのInvalidPartエラーも修正した。
それぞれ回帰テストを追加してから実環境へ配置した。

## 待受時の実測

Gateway認証後、SSMで状態を確認している時点の値。
イベント配信の負荷試験はまだ実施していない。

| 指標 | 実測値 |
| --- | --- |
| GatewayのRSS | 22,716KiB、約22.2MiB |
| ホストのMemTotal | 432,320,512bytes、約412.3MiB |
| ホストのMemAvailable | 195,104,768bytes、約186.1MiB |
| CloudWatch Agentのcgroup MemoryCurrent | 56,180,736bytes、約53.6MiB |

RSSとcgroupのMemoryCurrentは異なる指標として記載した。
OS既定のzramは有効で、パッケージ導入時だけ使った一時ディスクswapは削除済み。

## 接続先

- 管理API：http://10.203.0.1:8080
- Workerの結果通知：https://discord-gateway.chigayuki.com/results
- [CloudWatchダッシュボード](https://ap-northeast-1.console.aws.amazon.com/cloudwatch/home?region=ap-northeast-1#dashboards/dashboard/discord-management-bot)

PC用の設定例は.local/wireguard-client.conf.exampleに出力済み。
PCからの管理API疎通は、利用者がWireGuardを接続してから確認する。
初回構築時点では、PDFプレビュー・Timelineのトリガー登録と移行は未実施だった。

## 2026-11-15の更新

配信一覧と詳細にresult_statusを追加した。
保存済み結果のstatusから算出し、最終結果が未確定ならnullを返す。
一覧のSQLでは配信本文と結果全文を取得しない。
既存のPDF配信5件でも、一覧の要約と詳細の最終結果が一致することを確認した。
改名後のdiscord-pdf-previewへの配信にもsuccessの記録がある。

Skillに配信・結果通知の契約を同梱し、本文なしDELETEのContent-Type、候補delivery_ids、検証段階ごとの副作用、接続確認のタイムアウトを補足した。
リポジトリとインストール済みSkillの双方へ反映した。

go test -race ./...、Skillの標準検証、参照リンクと正本文書の一致確認が成功した。
Ansibleのapplication更新は失敗0件で完了し、配置バイナリのSHA-256がビルド成果物と一致した。
Gateway・WireGuard・CloudWatch Agent・backup timerの稼働を確認した。

実環境ではWireGuard上の一時受信先で署名を検証し、202の受付と署名付き結果通知を往復させた。
success・partial_success・failure・skipped、未確定時のnull、一覧の本文省略、条件不一致時の候補ID、本文なしDELETEについて29項目を確認した。
検証用の配信先・トリガーは削除済みで、検証の配信履歴は通常の保持方針に従う。
既存PDF設定を保持し、未受付・結果待ち・イベント待ちは0件だった。
