package workers

import (
	"context"
	"errors"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/config"
	"example.com/wagering/internal/messaging"
	"example.com/wagering/internal/postgres"
	"example.com/wagering/internal/telemetry"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgconn"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

type Workers struct {
	cfg      config.Config
	broker   *messaging.Broker
	store    *postgres.Store
	service  *application.Service
	metrics  *telemetry.Metrics
	log      *slog.Logger
	poll     context.Context
	stopPoll context.CancelFunc
	work     context.Context
	stopWork context.CancelFunc
	done     chan struct{}
	wg       sync.WaitGroup
}

func New(c config.Config, b *messaging.Broker, p *postgres.Store, s *application.Service, m *telemetry.Metrics, l *slog.Logger) *Workers {
	return &Workers{cfg: c, broker: b, store: p, service: s, metrics: m, log: l}
}
func (w *Workers) Start() {
	w.poll, w.stopPoll = context.WithCancel(context.Background())
	w.work, w.stopWork = context.WithCancel(context.Background())
	w.done = make(chan struct{})
	launch := func(fn func()) { w.wg.Add(1); go func() { defer w.wg.Done(); fn() }() }
	for i := 0; i < w.cfg.Workers; i++ {
		launch(w.consume)
	}
	launch(w.publish)
	launch(w.references)
	launch(w.measure)
	go func() { w.wg.Wait(); close(w.done); w.log.Info("all workers stopped") }()
}
func (w *Workers) StopFetching() {
	if w.stopPoll != nil {
		w.stopPoll()
	}
}
func (w *Workers) Stop(ctx context.Context) error {
	w.StopFetching()
	if w.done == nil {
		return nil
	}
	select {
	case <-w.done:
		w.stopWork()
		return nil
	case <-ctx.Done():
		w.stopWork()
	}
	select {
	case <-w.done:
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return errors.New("workers exceeded shutdown deadline")
	}
}
func delay(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func failpoint(name string) {
	// Deliberately process-wide, local test-only. Never exposed through HTTP.
	if os.Getenv("ALLOW_FAILPOINTS") == "true" && os.Getenv("FAILPOINT") == name {
		os.Exit(86)
	}
}
func (w *Workers) consume() {
	for w.poll.Err() == nil {
		r, err := w.broker.Client.ReceiveMessage(w.poll, &sqs.ReceiveMessageInput{QueueUrl: aws.String(w.broker.Input), MaxNumberOfMessages: 1, WaitTimeSeconds: 10, VisibilityTimeout: 30, MessageAttributeNames: []string{"providerSignature"}, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
		if err != nil {
			if w.poll.Err() != nil {
				return
			}
			w.metrics.Retries.Add(1)
			w.log.Warn("SQS receive retry")
			if !delay(w.poll, time.Second) {
				return
			}
			continue
		}
		for _, m := range r.Messages {
			w.handle(m)
		}
	}
}
func (w *Workers) visibility(m types.Message, seconds int32) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := w.broker.Client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(w.broker.Input), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: seconds}); err != nil {
		w.log.Warn("visibility release failed; waiting for timeout")
	}
}
func (w *Workers) handle(m types.Message) {
	if w.poll.Err() != nil {
		w.visibility(m, 0)
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(w.work, 10*time.Second)
	defer cancel()
	signature := aws.ToString(m.MessageAttributes["providerSignature"].StringValue)
	in, err := messaging.Decode(aws.ToString(m.Body), signature, w.cfg.SigningKeys)
	var out application.Result
	if err == nil {
		out, err = w.service.Execute(ctx, in)
	}
	if err != nil {
		w.metrics.Retries.Add(1)
		count, _ := strconv.Atoi(m.Attributes["ApproximateReceiveCount"])
		seconds := int32(application.Backoff(count) / time.Second)
		if w.poll.Err() != nil || w.work.Err() != nil {
			seconds = 0
		}
		// Invalid/permanent inputs are never ACKed: broker redrive sends them to DLQ.
		w.visibility(m, seconds)
		w.log.Warn("SQS treatment retry", "messageId", in.MessageID, "providerId", in.Request.ProviderID, "walletId", in.Request.WalletID, "attempt", count)
		return
	}
	w.metrics.Observe(string(out.Status), out.Replay, start)
	w.log.Info("SQS treatment committed", "messageId", in.MessageID, "correlationId", in.CorrelationID, "transactionId", out.TransactionID, "walletId", in.Request.WalletID, "providerId", in.Request.ProviderID, "replay", out.Replay)
	failpoint("after_commit_before_ack")
	if _, err = w.broker.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(w.broker.Input), ReceiptHandle: m.ReceiptHandle}); err != nil {
		w.metrics.Retries.Add(1)
		w.log.Warn("SQS ack failed; safe to redeliver", "messageId", in.MessageID)
	}
}
func (w *Workers) publish() {
	for w.poll.Err() == nil {
		ctx, cancel := context.WithTimeout(w.work, 10*time.Second)
		o, err := w.store.Claim(ctx)
		if err == nil && o != nil {
			failpoint("after_outbox_claim")
			_, err = w.broker.Client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(w.broker.Output), MessageBody: aws.String(string(o.Payload)), MessageGroupId: aws.String(o.Aggregate), MessageDeduplicationId: aws.String(o.ID)})
			if err == nil {
				failpoint("after_publish_before_mark")
				err = w.store.Published(ctx, o)
			} else {
				_ = w.store.Retry(ctx, o)
			}
		}
		cancel()
		if err != nil {
			w.metrics.Retries.Add(1)
			w.log.Warn("outbox retry")
		}
		if err != nil || o == nil {
			if !delay(w.poll, 300*time.Millisecond) {
				return
			}
		}
	}
}
func (w *Workers) references() {
	for w.poll.Err() == nil {
		ctx, cancel := context.WithTimeout(w.work, 2*time.Second)
		ids, err := w.store.Due(ctx)
		cancel()
		if err != nil {
			w.log.Warn("reference scan retry")
			w.metrics.Retries.Add(1)
		}
		for _, id := range ids {
			if w.poll.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(w.work, 10*time.Second)
			started := time.Now()
			out, err := w.service.ResumeResult(ctx, id)
			if err == nil && out != nil {
				w.metrics.Observe(string(out.Status), false, started)
			}
			cancel()
			if permanentStorage(err) {
				failureCtx, stop := context.WithTimeout(w.work, 5*time.Second)
				markErr := w.service.FailPending(failureCtx, id)
				stop()
				if markErr == nil {
					w.metrics.Failed.Add(1)
					w.log.Error("pending transaction permanently failed", "transactionId", id)
					continue
				}
			}
			if err != nil {
				w.metrics.Retries.Add(1)
				w.log.Warn("reference treatment retry", "transactionId", id)
			}
		}
		if !delay(w.poll, time.Second) {
			return
		}
	}
}
func (w *Workers) measure() {
	for w.poll.Err() == nil {
		ctx, cancel := context.WithTimeout(w.work, 3*time.Second)
		if n, err := w.store.OutboxAge(ctx); err == nil {
			w.metrics.OutboxAge.Store(n)
		}
		if attrs, err := w.broker.Client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(w.broker.DLQ), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}}); err == nil {
			n, _ := strconv.ParseInt(attrs.Attributes["ApproximateNumberOfMessages"], 10, 64)
			w.metrics.DLQ.Store(n)
		}
		cancel()
		if !delay(w.poll, 5*time.Second) {
			return
		}
	}
}

func permanentStorage(err error) bool {
	var pg *pgconn.PgError
	// Explicit allowlist. Connectivity, deadlock, serialization and commit ambiguity retry.
	return errors.As(err, &pg) && (pg.Code == "22003" || pg.Code == "22023")
}
