# 既存SSMと鍵のTerraformへの引継ぎ

この手順はレビュー用であり、実環境へのimportや適用はまだ実施しない。
停止済みEC2を起動する操作も含めない。

既存のsigning-seedとwireguard-server-keyを、新しい乱数で置き換えてはならない。
Workerが信頼する署名公開鍵や、PCのWireGuard設定が変わるためである。

承認後の引継ぎでは、次の対応で既存の値をstateへ取り込む。

| 取り込むリソース | 既存値 |
| --- | --- |
| aws_ssm_parameter.signing_seed | /discord-management-bot/signing-seed |
| aws_ssm_parameter.wireguard_server_key | /discord-management-bot/wireguard-server-key |
| random_bytes.signing_seed | 上記seedを標準Base64へ戻した32バイトの値 |
| random_bytes.wireguard_server_key | 上記WireGuard秘密鍵の標準Base64値 |

[random_bytesは既存Base64値のimportに対応する](https://registry.terraform.io/providers/hashicorp/random/latest/docs/resources/bytes#import)。
SSMだけをimportして生成鍵を新規作成すると、次のplanで秘密鍵の変更が出るため、両方を取り込む。
値は画面や共有ログへ出力しない。

既存のbot-token・guild-id・discord-webhook-url・wireguard-client-public-keyがある場合も、同名のaws_ssm_parameterリソースへimportする。
未作成のものは、Terraform入力から作成する。
手動のSSM登録・overwriteによる強制上書き・valueのignore_changesは使わない。

取り込み後は、既存の署名seedとWireGuard秘密鍵に変更がないこと、EC2に起動や置換の差分がないことをplanで確認してから適用を判断する。
旧wireguard-server-publicパラメーターは新しい構成では参照しない。
EC2から取得した公開鍵をPC用configへ反映するため、公開鍵の複製を別途更新する必要はない。

## 使用しないartifact用S3の整理

今後の構成はローカルdistをAnsibleで配置するため、artifact bucketは新設しない。
既存のbucket・公開制限・暗号化・versioning・TLS policyは、retired-artifacts.tfのmovedとremovedにより、destroy=falseでstateの管理対象から外す。
承認後にこの移行を適用しても、既存bucketの内容や保護設定を削除しない。
実資源の廃止と保存内容の削除は、このstate移行とは別の明示的な操作として扱う。
