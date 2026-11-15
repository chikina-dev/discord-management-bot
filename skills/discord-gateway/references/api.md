# 管理API

管理APIはWireGuard側のHTTPリスナーでのみ公開する。
公開HTTPSリスナーには結果通知用POST /resultsだけを登録する。
GET・HEAD以外は、本文の有無にかかわらずContent-Type: application/jsonを付ける。
本文なしのDELETEやPOSTでも必要であり、付け忘れた場合は415を返す。
ブラウザーからの変更操作は受け付けない。

## 接続

管理APIのURLを指定し、curlなどのHTTPクライアントで直接操作する。
本番の接続先は利用者が設定したWireGuard側のアドレスを使う。

```sh
curl --fail-with-body --connect-timeout 5 --max-time 10 http://10.203.0.1:8080/status
curl --fail-with-body --connect-timeout 5 --max-time 10 http://10.203.0.1:8080/triggers \
  --header 'Content-Type: application/json' --data-binary @trigger.json
curl --fail-with-body --connect-timeout 5 --max-time 10 \
  --request DELETE --header 'Content-Type: application/json' \
  http://10.203.0.1:8080/triggers/tr_REPLACE_WITH_TARGET_ID
```

JSONはファイルから--data-binaryで渡せる。
レスポンスの保存には--outputを使う。

## エンドポイント

| メソッド | パス | 内容 |
| --- | --- | --- |
| GET | /status | Guild ID、Gateway接続状態、配信状況 |
| GET | /signing-key | EC2の署名公開鍵とkey_id |
| GET | /capabilities | 条件式で使える演算子・メソッド・上限 |
| POST | /gateway/reconnect | 接続を再確立する。保存済みSessionがあればResumeを試す |
| GET / POST | /destinations | 配信先の一覧・登録 |
| GET / PUT / DELETE | /destinations/{id} | 配信先の取得・置換・削除 |
| GET / POST | /triggers | トリガーの一覧・登録 |
| GET / PUT / DELETE | /triggers/{id} | トリガーの取得・置換・削除 |
| POST | /rules/evaluate | 保存も配信も行わず、条件式を検証 |
| POST | /tests/events | 指定したトリガーだけに検証イベントを投入 |
| GET | /deliveries | 配信履歴の一覧 |
| GET | /deliveries/{id} | 配信本文と処理結果を含む詳細 |
| POST | /deliveries/{id}/cancel | 未受付分の再送を取消 |

一時停止・無効化のエンドポイントやenabledフラグは設けない。
トリガーを止める場合は削除する。
配信先の削除は関連トリガーも一括削除する。
履歴と送信済み分の結果通知は設定削除後も保持する。

## 配信先

作成はPOST /destinations、更新はPUT /destinations/{id}を使う。
PUTでは以下の3項目をすべて指定する。

```json
{
  "name": "pdf-preview",
  "url": "https://your-worker.example/events",
  "result_public_key": "Workerが保持するEd25519秘密鍵に対応する32バイト公開鍵のBase64URL"
}
```

公開鍵にパディングは付けない。
Bot TokenやWorkerの秘密鍵はこのAPIへ登録しない。
URL・公開鍵の更新は初回送信前の配信から適用する。送信済み分の再送と結果検証には初回送信時のURL・公開鍵を使い、削除時もその履歴を書き換えない。
公開鍵更新は送信済み配信の即時失効ではない。
HTTPSを使う。WireGuard内やループバック上の検証用受信先ではHTTPも使える。

## トリガー

作成はPOST /triggers、更新はPUT /triggers/{id}を使う。
PUTでは以下の3項目をすべて指定する。

```json
{
  "name": "human-pdf",
  "destination_id": "dst_…",
  "condition": "event_type == \"MESSAGE_CREATE\" && data.author.bot != true && data.webhook_id == null && data.attachments.some(a => a.content_type == \"application/pdf\" || a.filename.toLowerCase().endsWith(\".pdf\"))"
}
```

Botフラグは省略されることもあるため、人間の投稿を対象にする条件にはbot != trueを使える。
GET /triggers?destination_id=dst_…で配信先を絞り込める。
条件の構文・正規表現は保存前に検証し、不正な更新は既存設定を変更しない。

## 配信しない条件評価

POST /rules/evaluateはcondition・event_type・data・contextを受け取る。
match、attachment_indices、uses_attachmentsと、必要に応じて選別済みattachmentsを返す。
保存・HTTP配信・Discordへの投稿は行わない。

## 往復テスト

POST /tests/eventsにはevent_type・data・context・trigger_idsを渡す。
trigger_idsは必須であり、指定したトリガー以外には配信しない。
テスト用配信先と専用トリガーを作って使う。
実際にHTTP配信するため、既存の実利用Workerへ投入する場合は、その送信自体が利用者の依頼に含まれていることを確認する。

202は検証イベントの保存を意味し、条件評価とWorkerへの配信はその後に行う。
レスポンスのevent_idは保存したイベントのID、delivery_idsは指定トリガーごとに算出した配信候補IDである。
delivery_idsが返っても配信レコードの作成は保証しない。
条件不一致では、その候補IDの配信レコードは作られない。

```json
{
  "event_id": "ev_…",
  "delivery_ids": ["dl_…"]
}
```

GET /deliveries/{id}の404だけでは、評価待ちと条件不一致を区別できない。
現時点でイベントの評価完了状態を取得するAPIはない。
事前に/rules/evaluateで条件を確かめ、結果を待つ場合は確認期限を決める。
期限までに確認できなければ未確認として報告し、候補IDの存在を無期限に待たない。

context.sourceにはEC2がtestを付ける。元のdataは変更しない。
この値は識別用であり、Workerの処理・PDF取得・Discord投稿を抑止しない。

## 検証の段階と副作用

| 段階 | 例 | 副作用 |
| --- | --- | --- |
| 条件評価だけ | /rules/evaluateでPDF2件が選ばれるか確認 | 保存・配信・Discord投稿なし |
| 検証用Workerとの往復 | 専用配信先・専用トリガーを/tests/eventsで指定 | 配信・結果が保存され、実際にHTTP通信する |
| 本番Workerの処理確認 | 利用者が指定したチャンネルへPDFを投稿し、配信履歴を追う | PDF取得・変換・Discord投稿など、本番Workerの処理が実行される |

条件評価だけなら、例えば次を実行する。
応答のmatchがtrue、attachment_indicesが[0,1]なら、PDF2件が選ばれている。

```sh
curl --fail-with-body --connect-timeout 5 --max-time 10 \
  http://10.203.0.1:8080/rules/evaluate \
  --header 'Content-Type: application/json' --data-binary @- <<'JSON'
{
  "condition": "data.attachments.some(a => a.content_type == \"application/pdf\")",
  "event_type": "MESSAGE_CREATE",
  "data": {
    "attachments": [
      {"id": "1", "content_type": "application/pdf"},
      {"id": "2", "content_type": "application/pdf"},
      {"id": "3", "content_type": "image/png"}
    ]
  },
  "context": {}
}
JSON
```

往復テストでは、Discord投稿を実装しない専用の検証用Workerを用意する。
署名検証・202応答・署名付き結果通知は通常の接続契約に従う。
その配信先を参照する検証トリガーのIDだけを指定する。
次の例では、tr_REPLACE_WITH_TEST_TRIGGER_IDを自分が作った検証トリガーへ置き換える。

```sh
curl --fail-with-body --connect-timeout 5 --max-time 10 \
  http://10.203.0.1:8080/tests/events \
  --header 'Content-Type: application/json' --data-binary @- <<'JSON'
{
  "event_type": "MESSAGE_CREATE",
  "data": {"case": "round-trip"},
  "context": {},
  "trigger_ids": ["tr_REPLACE_WITH_TEST_TRIGGER_ID"]
}
JSON
```

本番Workerの確認では、利用者が指定した投稿先と処理内容に限定する。
例えばPDFプレビューの実投稿なら、指定されたチャンネルにPDF1件を投稿し、対象配信のresult_statusがsuccessであることと、実際のプレビュー投稿を確認する。
期限切れイベントでskippedを返すかどうかはWorker固有の実装であり、汎用の無副作用テストとしては使わない。

## 履歴

GET /deliveriesはdestination_id・state・limit・beforeで絞り込める。
limitの最大は100。beforeには直前ページのnext_beforeを指定する。
一覧は配信本文のpayloadとWorkerの最終結果全文のresultを省略する。
result_statusには、保存済みの最終結果のstatusを返す。
最終結果が未確定ならnull、確定後はsuccess・partial_success・failure・skippedのいずれか。
例えばstateがfinishedでresult_statusがfailureなら、失敗という最終結果の記録が完了している。
HTTP 202の受付やstateだけを成功の根拠にしない。
詳細のGET /deliveries/{id}ではresult_statusに加え、payloadとresultも取得できる。
大きい本文の保存にはcurlの--outputを使える。

stateはpending・accepted・stopped・finished。
Workerの最終結果はsuccess・partial_success・failure・skipped。
stoppedはEC2が未受付分の送信を止めた状態であり、Workerの失敗として扱わない。

受信できるイベントは、受信用BotのGateway IntentsとDiscord上の閲覧権限に従う。
条件式の登録だけでは、未受信のイベントを取得できない。
