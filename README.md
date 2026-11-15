# Discord Management Bot

Discordサーバー「times_ちきな(裏)」のGatewayイベントを、SQLiteに保存したトリガーで照合し、各Cloudflare WorkerへHTTP POSTする。
Discordへの投稿は各Workerが自分のBot Tokenで行い、処理結果をGatewayへ通知する。

## 管理

WireGuard接続後、[discord-gateway Skill](skills/discord-gateway/SKILL.md)で配信先・トリガーを登録・変更・削除する。
配信先を登録し、そのIDを参照するトリガーに条件式を設定する。
管理APIは`http://10.203.0.1:8080`。

## 仕様

- [管理API](docs/management-api.md)
- [条件式](docs/conditions.md)
- [Workerへの配信と結果通知](docs/protocol.md)
- [設計](docs/design.md)
