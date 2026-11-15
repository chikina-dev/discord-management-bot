package cloud

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	dnsTypes "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

var ErrMissing = errors.New("parameter not found")

type Clients struct {
	SSM *ssm.Client
	S3  *s3.Client
	DNS *route53.Client
}

func New(ctx context.Context, region string) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &Clients{SSM: ssm.NewFromConfig(cfg), S3: s3.NewFromConfig(cfg), DNS: route53.NewFromConfig(cfg)}, nil
}
func (c *Clients) Get(ctx context.Context, parameter string) (string, error) {
	name := "/discord-management-bot/" + parameter
	out, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil {
		var missing *ssmTypes.ParameterNotFound
		if errors.As(err, &missing) {
			return "", ErrMissing
		}
		return "", err
	}
	if out.Parameter == nil || out.Parameter.Value == nil || strings.TrimSpace(*out.Parameter.Value) == "" {
		return "", ErrMissing
	}
	return *out.Parameter.Value, nil
}
func PublicIPv4(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	req, _ := http.NewRequestWithContext(ctx, "PUT", "http://169.254.169.254/latest/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	token, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		return "", errors.New("IMDSv2 token request failed")
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", "http://169.254.169.254/latest/meta-data/public-ipv4", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(token))
	resp, err = client.Do(req)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		return "", errors.New("public IPv4 lookup failed")
	}
	ip := strings.TrimSpace(string(body))
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", errors.New("invalid public IPv4")
	}
	return ip, nil
}
func (c *Clients) UpdateDNS(ctx context.Context, zone, host, ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return errors.New("IPv4 required")
	}
	name := strings.TrimSuffix(host, ".") + "."
	list, err := c.DNS.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{HostedZoneId: aws.String(zone), StartRecordName: aws.String(name), StartRecordType: dnsTypes.RRTypeA, MaxItems: aws.Int32(1)})
	if err != nil {
		return err
	}
	if len(list.ResourceRecordSets) == 0 || aws.ToString(list.ResourceRecordSets[0].Name) != name || list.ResourceRecordSets[0].Type != dnsTypes.RRTypeA {
		return ErrMissing
	}
	_, err = c.DNS.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{HostedZoneId: aws.String(zone), ChangeBatch: &dnsTypes.ChangeBatch{Changes: []dnsTypes.Change{{Action: dnsTypes.ChangeActionUpsert, ResourceRecordSet: &dnsTypes.ResourceRecordSet{Name: aws.String(name), Type: dnsTypes.RRTypeA, TTL: aws.Int64(60), ResourceRecords: []dnsTypes.ResourceRecord{{Value: aws.String(ip)}}}}}}})
	return err
}
