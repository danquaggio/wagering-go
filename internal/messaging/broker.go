package messaging

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"example.com/wagering/internal/application"
	appconfig "example.com/wagering/internal/config"
	"example.com/wagering/internal/domain"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"io"
	"time"
)

type Broker struct {
	Client             *sqs.Client
	Input, Output, DLQ string
	cfg                appconfig.Config
}

func New(c appconfig.Config) (*Broker, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(c.Region))
	if err != nil {
		return nil, err
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	})
	return &Broker{Client: client, cfg: c}, nil
}
func (b *Broker) Init(ctx context.Context) error {
	for name, p := range map[string]*string{b.cfg.QueueName: &b.Input, b.cfg.OutputName: &b.Output, b.cfg.DLQName: &b.DLQ} {
		r, err := b.Client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			return err
		}
		*p = aws.ToString(r.QueueUrl)
	}
	return b.Ready(ctx)
}
func (b *Broker) Ready(ctx context.Context) error {
	for _, q := range []string{b.Input, b.Output, b.DLQ} {
		if q == "" {
			return errors.New("queues not initialized")
		}
		if _, err := b.Client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(q), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}}); err != nil {
			return err
		}
	}
	return nil
}

type RequestedData struct {
	domain.Request
	Key string `json:"idempotencyKey"`
}
type Requested struct {
	MessageID  string        `json:"messageId"`
	Type       string        `json:"type"`
	OccurredAt time.Time     `json:"occurredAt"`
	Data       RequestedData `json:"data"`
}

func Sign(body []byte, key string) string {
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
func Decode(body, signature string, keys map[string]string) (application.Input, error) {
	if len(body) > 64<<10 {
		return application.Input{}, domain.Rule("INVALID_MESSAGE")
	}
	var env Requested
	d := json.NewDecoder(bytes.NewBufferString(body))
	d.DisallowUnknownFields()
	d.UseNumber()
	if d.Decode(&env) != nil {
		return application.Input{}, domain.Rule("INVALID_MESSAGE")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return application.Input{}, domain.Rule("INVALID_MESSAGE")
	}
	if !domain.ValidToken(env.MessageID) || env.Type != "WagerTransactionRequested" || env.OccurredAt.IsZero() {
		return application.Input{}, domain.Rule("INVALID_MESSAGE")
	}
	key, ok := keys[env.Data.ProviderID]
	received, err := hex.DecodeString(signature)
	expected, hexerr := hex.DecodeString(Sign([]byte(body), key))
	if !ok || err != nil || hexerr != nil || !hmac.Equal(received, expected) {
		return application.Input{}, domain.Rule("UNAUTHORIZED_PRODUCER")
	}
	business, err := env.Data.Request.Hash()
	if err != nil {
		return application.Input{}, err
	}
	if !domain.ValidToken(env.Data.Key) {
		return application.Input{}, domain.Rule("INVALID_IDEMPOTENCY_KEY")
	}
	// Stable envelope hash, including metadata + actual key; no map of untyped JSON numbers.
	canonical, err := json.Marshal(map[string]string{"messageId": env.MessageID, "type": env.Type, "occurredAt": env.OccurredAt.UTC().Format(time.RFC3339Nano), "businessHash": business, "idempotencyKey": env.Data.Key})
	if err != nil {
		return application.Input{}, err
	}
	hash := sha256.Sum256(canonical)
	return application.Input{Request: env.Data.Request, Key: env.Data.Key, MessageID: env.MessageID, MessageHash: hex.EncodeToString(hash[:]), CorrelationID: env.MessageID}, nil
}
