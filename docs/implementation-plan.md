# 実装・検証計画

- [x] 配信・結果通知の契約と管理APIを定義する。
- [x] Goの条件式パーサー・評価器、SQLite、Gateway接続、HTTP配信・結果通知、管理APIを実装する。
- [x] 外部キーによる削除連動、履歴保持、再送・重複通知・再起動時の復旧を検証する。
- [x] TerraformでEC2・専用EBS・IAM・S3・Route 53・SSMパラメーター管理・CloudWatch・EventBridge通知を構成する。
- [ ] 利用者が設定したWireGuard経由で管理APIを操作するSkillを実装・検証する。
- [ ] 実際のt4g.nanoに全構成を載せ、通常稼働・負荷・バックアップ時のCPU・メモリ・処理時間を実測する。

PDFプレビュー側の変更・移行はこの作業に含めない。
Bot Tokenと通知Webhook URLの値はリポジトリに保存しない。


EC2の構成はAnsibleで管理する。PC側のWireGuard読み込み・接続操作は利用者が担当する。

- [ ] 既存SSM・鍵のTerraform引継ぎをレビューする。実行は別途承認後に行う。
- [x] Terraform・Ansibleの再設計をモックとローカル検証で確認する。

## 全体レビューの修正

- [x] カタログの保存前snapshot準備とcommit後のI/Oなし公開。
- [x] イベントcontextの永続captureとチャンネル観測順序の保持。
- [x] 既存DBだけを扱う容量制限付きbackupと復旧のwriter排他。
- [x] EC2交換時の永続ファイルUID/GID整合。
- [x] 結果JSON数値の精度・表記保持。
- [x] 変更対象別activationとsystemdの起動失敗回復。
- [x] 初回送信時のURL・鍵固定と結果記録時の原子的な鍵照合。
- [x] 未使用artifact宣言の削除とdestroy=falseのstate移行。

これらはローカル実装と検証であり、実環境へは未反映。
