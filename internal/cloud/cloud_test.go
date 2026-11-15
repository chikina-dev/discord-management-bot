package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

func fixtureAWS(client *http.Client) aws.Config {
	return aws.Config{
		Region: "ap-northeast-1", HTTPClient: client, RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "fixture", SecretAccessKey: "fixture"}, nil
		}),
	}
}

func TestGetRequiresNonemptyParameter(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		status         int
		missing        bool
	}{
		{"present", `{"Parameter":{"Value":"receiver-value"}}`, 200, false},
		{"absent", `{"__type":"ParameterNotFound","message":"not found"}`, 400, true},
		{"empty", `{"Parameter":{"Value":""}}`, 200, true},
		{"whitespace", `{"Parameter":{"Value":" \t\n"}}`, 200, true},
		{"missing_value", `{"Parameter":{}}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Name           string
					WithDecryption bool
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Name != "/discord-management-bot/bot-token" || !request.WithDecryption {
					t.Errorf("unexpected SSM request: %+v, %v", request, err)
				}
				w.Header().Set("Content-Type", "application/x-amz-json-1.1")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			clients := Clients{SSM: ssm.NewFromConfig(fixtureAWS(server.Client()), func(o *ssm.Options) {
				o.BaseEndpoint = aws.String(server.URL)
			})}
			value, err := clients.Get(context.Background(), "bot-token")
			if tc.missing {
				if !errors.Is(err, ErrMissing) {
					t.Fatalf("required parameter accepted: value=%q error=%v", value, err)
				}
			} else if err != nil || value != "receiver-value" {
				t.Fatalf("valid parameter rejected: value=%q error=%v", value, err)
			}
		})
	}
}

func TestDNSUpdateRequiresExistingTerraformRecord(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "missing"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			var updates atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/xml")
				switch r.Method {
				case http.MethodGet:
					records := ""
					if existing {
						records = "<ResourceRecordSet><Name>discord-gateway.chigayuki.com.</Name><Type>A</Type></ResourceRecordSet>"
					}
					_, _ = w.Write([]byte("<ListResourceRecordSetsResponse><ResourceRecordSets>" + records + "</ResourceRecordSets><IsTruncated>false</IsTruncated><MaxItems>1</MaxItems></ListResourceRecordSetsResponse>"))
				case http.MethodPost:
					updates.Add(1)
					var body strings.Builder
					_, _ = io.Copy(&body, r.Body)
					if !strings.Contains(body.String(), "203.0.113.10") || !strings.Contains(body.String(), "UPSERT") {
						t.Errorf("unexpected DNS update: %s", body.String())
					}
					_, _ = w.Write([]byte("<ChangeResourceRecordSetsResponse><ChangeInfo><Id>/change/fixture</Id><Status>PENDING</Status><SubmittedAt>2026-11-13T00:00:00Z</SubmittedAt></ChangeInfo></ChangeResourceRecordSetsResponse>"))
				default:
					t.Errorf("unexpected HTTP method: %s", r.Method)
				}
			}))
			defer server.Close()
			clients := Clients{DNS: route53.NewFromConfig(fixtureAWS(server.Client()), func(o *route53.Options) {
				o.BaseEndpoint = aws.String(server.URL)
			})}
			err := clients.UpdateDNS(context.Background(), "ZFIXTURE", "discord-gateway.chigayuki.com", "203.0.113.10")
			if !existing {
				if !errors.Is(err, ErrMissing) || updates.Load() != 0 {
					t.Fatalf("created a record before Terraform: error=%v updates=%d", err, updates.Load())
				}
			} else if err != nil || updates.Load() != 1 {
				t.Fatalf("existing record update failed: error=%v updates=%d", err, updates.Load())
			}
		})
	}
}
