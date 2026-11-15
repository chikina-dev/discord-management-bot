# EC2候補の比較

2026-11-12にAWS EC2 APIとAWS Pricing APIで確認した。
対象は東京リージョンap-northeast-1、Linux、共有テナンシー、オンデマンドとする。
月額は730時間稼働で計算し、無料枠・税・EBS・公開IPv4・CloudWatch・転送・追加CPUクレジットなどは含めない。
インスタンスタイプは未確定。

## 月額比較

以下の候補はいずれも2 vCPU。
列のファミリー名と行のサイズを組み合わせた名前がインスタンスタイプになる。

| サイズ | メモリ | T4g・ARM64 | T3a・x86_64 | T3・x86_64 | T8i・x86_64 |
| --- | --- | ---: | ---: | ---: | ---: |
| nano | 512 MiB | $3.94 | $4.45 | $4.96 | $5.96 |
| micro | 1 GiB | $7.88 | $8.91 | $9.93 | $11.91 |
| small | 2 GiB | $15.77 | $17.89 | $19.86 | $23.83 |

## 取得した時間単価

| サイズ | T4g | T3a | T3 | T8i |
| --- | ---: | ---: | ---: | ---: |
| nano | $0.0054 | $0.0061 | $0.0068 | $0.00816 |
| micro | $0.0108 | $0.0122 | $0.0136 | $0.01632 |
| small | $0.0216 | $0.0245 | $0.0272 | $0.03264 |

料金の取得条件はAmazonEC2、location = Asia Pacific (Tokyo)、instanceType = 各候補、operatingSystem = Linux、tenancy = Shared、preInstalledSw = NA、capacitystatus = Usedとした。
T8iのnano・micro・smallはdescribe-instance-type-offeringsでも東京での提供を確認した。

## 判断材料

- t4g.nanoでの稼働を前提に検討する。メモリ512 MiBを予算に常駐プロセスやバックアップ処理を設計し、稼働時のメモリ使用量を実測する。
- 最初のt4g.micro推奨は余裕を取る提案であり、nanoでは動かせないという実測根拠はない。
- t4g.smallはmicroより月約$7.88高く、メモリを2 GiBに増やせる。
- x86_64を選ぶ場合の安価な候補はt3a.micro。T8iは新しいIntel CPUを使う候補で、今回のアプリにおける速度比較は未実施。
- 各ファミリーのCPUベースラインはnanoが各vCPUの5%、microが10%、smallが20%。CPUクレジットにより一時的にこれを超えて動作する。
- 公開IPv4を1個使う場合、その料金は$0.005/時、730時間で$3.65となる。

参照: [CPUクレジットとベースライン](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/burstable-credits-baseline-concepts.html)、[T8iの仕様](https://aws.amazon.com/ec2/instance-types/t8i/)、[公開IPv4の料金](https://aws.amazon.com/vpc/pricing/)

## IPv4とIPv6の確認

2026-11-12にCloudflareとGoogleの公開DNSで確認したところ、gateway.discord.ggとdiscord.comはいずれもAレコードがあり、AAAAレコードはなかった。
この接続先へIPv6のみで直接接続する構成にはできず、IPv4への中継・変換を含む通信経路が必要となる。

- 公開IPv4の料金は固定のElastic IPでも自動割り当てでも$0.005/時。固定をやめても公開IPv4自体の料金はなくならない。
- 固定IPv4は使わず、自動割り当ての公開IPv4を使う。Discord Gatewayへの接続自体に固定IPv4は必要ない。
- 対象アカウントの東京リージョンには、利用可能な既存NAT Gatewayはなかった。
- AWS Pricing APIで確認した東京のNAT Gateway本体は$0.062/時、730時間で$45.26。データ処理や公開IPv4は別料金。
- IPv6-onlyからIPv4接続先へ到達させるAWS NAT64構成も可能だが、今回の単一EC2向けにNAT Gatewayを新設する案は、EC2自身へ公開IPv4を1個付ける案より費用が増える。
- 利用者は公開IPv4を使う構成を選択した。今回の要件にはIPv6併用は不要であり、IPv6は追加しない。
- SSM、CloudWatch Logs、CloudWatch Metricsのapi.awsエンドポイントとS3のdualstackエンドポイントにはAAAAレコードがあることを確認した。

参照: [AWSのDNS64・NAT64](https://docs.aws.amazon.com/vpc/latest/userguide/nat-gateway-nat64-dns64.html)、[DiscordのIPv6接続に関するIssue](https://github.com/discord/discord-api-docs/issues/6695)
