# 配信と結果通知の契約

配信スキーマにversionは設けない。

## 配信

EC2は配信先に登録したURLへJSONをPOSTする。
```json
{
  "delivery_id": "dl_…",
  "event_id": "ev_…",
  "trigger_id": "tr_…",
  "destination_id": "dst_…",
  "event_type": "MESSAGE_CREATE",
  "timestamps": {
    "ec2_received_at": "2026-11-12T01:00:00.000Z",
    "ec2_sent_at": "2026-11-12T01:00:00.010Z"
  },
  "data": {},
  "context": {}
}
```

- dataはDiscordイベント本体を保持する。元のフィールドを追加・削除・置換しない。
- context.channelにはEC2が保持・取得したチャンネル情報を置く。親フォーラムはcontext.channel.parent_idで参照する。
- 添付に関する条件を使う場合だけ、ルート直下のattachmentsに一致した添付オブジェクトを追加する。元のdata.attachmentsは変更しない。重複を除き元の順序を維持する。
- event_idはEC2が受信イベントに付けるID。delivery_idはトリガーごとの配信に付けるIDである。
- ec2_sent_atは最初の送信試行の時刻とする。再送でも配信本文とdelivery_idを維持し、認証用ヘッダーだけを更新する。
- 時刻はUTCのRFC 3339形式とし、ミリ秒まで記録する。

## 受付応答

Workerは署名とdestination_idを検証し、delivery_idを使って受付を永続化してから202を返す。
同じdelivery_idの再送では処理を再開せず、元の受付情報を返す。
同じIDで本文が変わった場合は409を返す。

```json
{
  "delivery_id": "dl_…",
  "timestamps": { "worker_accepted_at": "2026-11-12T01:00:00.020Z" }
}
```

202は処理の受付を意味する。Discordへの送信完了は次の結果通知で伝える。

## 最終結果

Workerはhttps://discord-gateway.chigayuki.com/resultsへ署名付きでPOSTする。
1つのdelivery_idに対応する処理全体について、最終結果を通知する。

```json
{
  "delivery_id": "dl_…",
  "destination_id": "dst_…",
  "status": "partial_success",
  "timestamps": {
    "worker_accepted_at": "2026-11-12T01:00:00.020Z",
    "worker_finished_at": "2026-11-12T01:00:01.000Z"
  },
  "result": { "succeeded": 2, "failed": 1 }
}
```

statusはsuccess、partial_success、failure、skippedの4種類とする。
resultはWorker固有の詳細を格納する任意のJSONであり、EC2はBot固有の判定を行わない。
EC2は受信時にec2_result_received_atを記録する。
成功時は204を返す。同じ最終結果の再通知も204とし、二重に記録・計上しない。
既に確定した結果と異なる内容は409とする。
同一性判定ではobjectのキー順と記録時刻の表現を正規化する。result内の数値は精度と表記を保持し、1と1.0など異なる表記を数学的に同値化しない。
Workerは受付応答を確認できるまで、同じ最終結果を新しい認証用ヘッダーで再送する。

## Ed25519認証

リクエストには次のヘッダーを付ける。

- X-Gateway-Key-Id: 公開鍵のSHA-256の先頭16バイトを小文字hexで表した識別子。
- X-Gateway-Timestamp: そのHTTP送信試行のUnix秒。
- X-Gateway-Signature: Ed25519署名のBase64URL表現。パディングは省略する。

署名対象は次の7行をLFで連結したUTF-8バイト列とする。最後にはLFを付けない。

```text
discord-management-gateway
<key-id>
<timestamp>
<大文字のHTTPメソッド>
<小文字のホスト名。既定以外のポートがあれば含める>
<エスケープ済みパスとクエリ。パスが空なら/>
<送信するJSON本文そのもののSHA-256を小文字hexで表した値>
```

受信側は署名と、認証用時刻が現在時刻の前後5分以内にあることを検証する。
これは5時点の記録時刻とは別であり、イベント自体の有効期限ではない。
JSONをパース・再構成した文字列ではなく、受信した本文のバイト列をハッシュする。

EC2の公開鍵は管理APIから取得する。Workerは自分に割り当てられたdestination_idも検証する。
結果通知にはWorker自身の秘密鍵を使い、EC2は配信先に登録された公開鍵と配信の対応を検証する。
公開鍵は32バイトのBase64URL、秘密鍵はEd25519の32バイトseedをBase64URLで表す。
HTTP通信にはHTTPSを使う。管理API・検証用の通信はWireGuard内で行える。


## 配信先のURL・公開鍵の更新

初回送信時に、その配信のURLと結果検証用公開鍵を確定する。
以後の再送は同じURL・公開鍵・本文・delivery_idを使う。
配信先のURL・公開鍵の更新は、まだ初回送信していない配信から適用する。

送信済みの配信には初回の公開鍵を使い、更新後の公開鍵ではその配信の結果を受け付けない。
EC2は署名検証した鍵IDと配信に確定した鍵IDの一致を、結果保存のDB更新条件で確認する。事前取得したsnapshotだけで認可しない。
逆に、旧公開鍵で新しい配信の結果を送ることもできない。
配信先の削除でも、送信済み配信の検証用公開鍵は書き換えない。
Workerは処理中配信の結果通知が受理されるまで、その配信で使う秘密鍵を保持する。

公開鍵の更新は、送信済み配信に対する即時失効操作ではない。
既に受け付けた処理の結果を失わずに記録する方針とする。
