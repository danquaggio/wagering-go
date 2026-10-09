//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"example.com/wagering/internal/auth"
	"example.com/wagering/internal/bootstrap"
	appconfig "example.com/wagering/internal/config"
	"example.com/wagering/internal/domain"
	"example.com/wagering/internal/messaging"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var client = &http.Client{Timeout: 15 * time.Second}

func token(t *testing.T, name string) string {
	t.Helper()
	endpoint := os.Getenv("TEST_TOKEN_URL")
	if endpoint == "" {
		t.Fatal("TEST_TOKEN_URL required")
	}
	response, err := client.PostForm(endpoint, url.Values{"grant_type": {"client_credentials"}, "client_id": {name}, "client_secret": {name + "-local-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Token string `json:"access_token"`
	}
	if err = json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != 200 || body.Token == "" {
		t.Fatal("IdP token request failed", response.StatusCode, err)
	}
	return body.Token
}
func apiURLs(t *testing.T) []string {
	t.Helper()
	urls := strings.Split(os.Getenv("TEST_API_URLS"), ",")
	if len(urls) != 3 {
		t.Fatal("TEST_API_URLS must contain three independent HTTP processes")
	}
	for _, base := range urls {
		until := time.Now().Add(90 * time.Second)
		for {
			resp, err := client.Get(base + "/health/ready")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(until) {
				t.Fatal("application not ready", base)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	return urls
}
func call(t *testing.T, base, method, path, bearer, key string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, base+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, out
}
func TestRealOIDCAndIsolation(t *testing.T) {
	urls := apiURLs(t)
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	in := input(t, w, domain.Bet, "25.00")
	for _, bearer := range []string{"", "invalid-token", token(t, "wrong-audience")} {
		if code, _ := call(t, urls[0], "POST", "/wagering/transactions", bearer, in.Key, in.Request); code != 401 {
			t.Fatal(code)
		}
	}
	a, b, internal := token(t, "provider-a"), token(t, "provider-b"), token(t, "wallet-service")
	if code, _ := call(t, urls[0], "GET", "/wallets/"+w.ID, a, "", nil); code != 403 {
		t.Fatal(code)
	}
	if code, _ := call(t, urls[0], "POST", "/wagering/transactions", b, in.Key, in.Request); code != 403 {
		t.Fatal(code)
	}
	if code, _ := call(t, urls[0], "POST", "/wagering/transactions", internal, in.Key, in.Request); code != 403 {
		t.Fatal(code)
	}
	assertWallet(t, s, w.ID, "100.00", 1)
	code, raw := call(t, urls[0], "POST", "/wagering/transactions", a, in.Key, in.Request)
	if code != 200 {
		t.Fatal(code, string(raw))
	}
	var result struct {
		ID string `json:"transactionId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if code, _ = call(t, urls[1], "GET", "/wagering/transactions/"+result.ID, b, "", nil); code != 404 {
		t.Fatal(code)
	}
	if code, _ = call(t, urls[1], "GET", "/providers/provider-a/wagering/transactions/"+in.Request.ExternalID, b, "", nil); code != 403 {
		t.Fatal(code)
	}
	if code, _ = call(t, urls[2], "POST", "/wagering/transactions", b, in.Key, in.Request); code != 403 {
		t.Fatal("cross-provider replay", code)
	}
	assertWallet(t, s, w.ID, "75.00", 2)

	cfg, err := appconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SigningKeys = map[string]string{"short-lived": "local-expiry-test-key-0000000000001"}
	verifier := auth.New(cfg)
	short := token(t, "short-lived")
	if _, err = verifier.Verify(context.Background(), "Bearer "+short); err != nil {
		t.Fatal("short token must be valid before expiration", err)
	}
	time.Sleep(3100 * time.Millisecond)
	if _, err = verifier.Verify(context.Background(), "Bearer "+short); err == nil {
		t.Fatal("expired token accepted")
	}
}
func sqsClient(t *testing.T, profile string) *sqs.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion("us-east-1"), awsconfig.WithSharedConfigProfile(profile))
	if err != nil {
		t.Fatal(err)
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(os.Getenv("SQS_ENDPOINT")) })
}
func queueURL(t *testing.T, c *sqs.Client, name string) string {
	t.Helper()
	r, err := c.GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		t.Fatal(err)
	}
	return aws.ToString(r.QueueUrl)
}
func eventually(t *testing.T, deadline time.Duration, fn func() bool) {
	t.Helper()
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func duplicates(t *testing.T, urls []string, bearer string) int64 {
	t.Helper()
	var n int64
	for _, base := range urls {
		code, body := call(t, base, "GET", "/metrics", bearer, "", nil)
		if code != 200 {
			t.Fatal(code)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "wager_duplicates_total ") {
				v, _ := strconv.ParseInt(strings.TrimPrefix(line, "wager_duplicates_total "), 10, 64)
				n += v
			}
		}
	}
	return n
}
func TestHTTPAndSQSRepeatedDeliveries(t *testing.T) {
	urls := apiURLs(t)
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	in := input(t, w, domain.Bet, "25.00")
	a, internal := token(t, "provider-a"), token(t, "wallet-service")
	if code, body := call(t, urls[0], "POST", "/wagering/transactions", a, in.Key, in.Request); code != 200 {
		t.Fatal(code, string(body))
	}
	producer := sqsClient(t, "provider-a")
	queue := queueURL(t, producer, "wager-transactions.fifo")
	env := messaging.Requested{MessageID: domain.ID(), Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC(), Data: messaging.RequestedData{Request: in.Request, Key: in.Key}}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	before := duplicates(t, urls, internal)
	for i := 0; i < 3; i++ {
		_, err = producer.SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(queue), MessageBody: aws.String(string(raw)), MessageGroupId: aws.String(w.ID),
			// New transport dedup ID deliberately bypasses the FIFO's five-minute dedup window.
			MessageDeduplicationId: aws.String(domain.ID()),
			MessageAttributes:      map[string]types.MessageAttributeValue{"providerSignature": {DataType: aws.String("String"), StringValue: aws.String(messaging.Sign(raw, "local-provider-a-signing-key-00000001"))}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 30*time.Second, func() bool { return duplicates(t, urls, internal) >= before+3 })
	var n int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM inbox WHERE message_id=$1 AND completed_at IS NOT NULL", env.MessageID).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	assertWallet(t, s, w.ID, "75.00", 2)
	eventually(t, 30*time.Second, func() bool {
		var n int
		err := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM outbox WHERE aggregate_id=$1 AND published_at IS NULL", w.ID).Scan(&n)
		return err == nil && n == 0
	})
}
func TestBrokerPolicyAndDLQ(t *testing.T) {
	_ = apiURLs(t)
	denied := sqsClient(t, "denied")
	if _, err := denied.GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String("wager-transactions.fifo")}); err == nil {
		t.Fatal("IAM deny not enforced")
	}
	producer := sqsClient(t, "provider-a")
	queue := queueURL(t, producer, "wager-transactions.fifo")
	if _, err := producer.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(queue)}); err == nil {
		t.Fatal("producer may not consume")
	}
	marker := domain.ID()
	body := fmt.Sprintf(`{"messageId":%q,"type":"INVALID"}`, marker)
	if _, err := producer.SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(queue), MessageBody: aws.String(body), MessageGroupId: aws.String(marker), MessageDeduplicationId: aws.String(marker)}); err != nil {
		t.Fatal(err)
	}
	reader := sqsClient(t, "event-reader")
	dlq := queueURL(t, reader, "wager-transactions-dlq.fifo")
	eventually(t, 150*time.Second, func() bool {
		r, err := reader.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(dlq), MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
		if err != nil {
			return false
		}
		for _, m := range r.Messages {
			if strings.Contains(aws.ToString(m.Body), marker) {
				_, err = reader.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(dlq), ReceiptHandle: m.ReceiptHandle})
				return err == nil
			}
		}
		return false
	})
}
func TestRealFxStartStop(t *testing.T) {
	_ = apiURLs(t)
	cfg, err := appconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Address = "127.0.0.1:0"
	var pool *pgxpool.Pool
	app := fx.New(bootstrap.Options(), fx.Replace(cfg), fx.Populate(&pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stop, cancelStop := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStop()
	if err = app.Stop(stop); err != nil {
		t.Fatal(err)
	}
	if err = pool.Ping(context.Background()); err == nil {
		t.Fatal("pool still open after shutdown")
	}
}
