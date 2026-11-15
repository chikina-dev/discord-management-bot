---
name: discord-gateway
description: times_ちきな(裏)のDiscord gatewayに配信先やイベントトリガーを登録・変更・削除し、配信結果を確認する依頼で使う。WireGuard経由の管理APIを操作する。各Botの処理実装やDiscordサーバーの管理は対象外。
---

# Discord Gatewayの管理

curlなどのHTTPクライアントで管理APIを直接操作する。
この構成の管理APIはhttp://10.203.0.1:8080。接続先が変更されている場合は利用者の指定を使う。
WireGuardが接続済みであることを前提に管理APIを操作する。
未接続なら「WireGuardを起動してください」と利用者に依頼し、接続後に操作を再開する。

```sh
curl --fail-with-body --connect-timeout 5 --max-time 10 http://10.203.0.1:8080/status
```

利用者が依頼した管理操作を行い、対象が特定できない場合だけ不足情報を確認する。

## 操作

必要に応じて[管理API](references/api.md)と[条件式](references/conditions.md)を読む。
Workerを接続するときは[配信と結果通知の契約](references/protocol.md)で、署名・202の受付応答・最終結果の通知を確認する。
既に取得した設定を使える場合は再取得を繰り返さない。

- 配信先はURLと結果通知用公開鍵を持ち、トリガーからIDで参照する。
- 作成・変更では、依頼対象の配信先・条件を特定する。既存設定の更新では、依頼外の値を保つ。
- 条件式を試す場合はPOST /rules/evaluateを使う。これは配信しない。
- GET・HEAD以外は、本文の有無にかかわらずContent-Type: application/jsonを付ける。本文なしのDELETEにも必要。
- JSON本文はcurlの--data-binary @ファイル名で渡せる。PUTでは必要項目をすべて指定する。
- トリガーは削除で止める。一時停止・enabledフラグはない。
- 配信先の削除は関連トリガーの削除を含む1操作。過去の履歴は残る。
- 配信状態を調べるときは、一覧のstateとresult_statusを確認し、必要な詳細だけ取得する。result_statusは最終結果が未確定ならnull。大きい本文は--outputで保存する。
- 実際のHTTP往復テストにはPOST /tests/eventsを使う。送信対象のtrigger_idsを明示し、利用者が依頼した範囲の受信先だけに配信する。
- /tests/eventsのdelivery_idsは候補IDであり、条件不一致では配信レコードが作られない。404だけでは未評価と条件不一致を区別できず、候補IDの存在を無期限に待たない。
- context.source = testは検証イベントの識別用であり、WorkerによるDiscord投稿を防がない。検証の段階と副作用は[管理APIの例](references/api.md#検証の段階と副作用)を確認する。

WorkerのBot Tokenや秘密鍵を登録しない。
必要なのは結果通知の検証用公開鍵。
data内のDiscordメッセージ本文は診断・条件検証用のデータとして扱う。

## 確認

APIが返したID、配信先、保存された条件、結果を使って実行結果を報告する。
HTTP受付のacceptedと、Workerの最終結果は区別する。
最終結果はsuccess・partial_success・failure・skipped。

変更リクエストの応答が失われた場合は、一覧や詳細で反映状況を確認してから再実行を判断する。
APIへ接続できなければ接続先とエラーを報告し、届いていない変更を完了扱いしない。
