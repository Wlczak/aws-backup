// Package restore implements the SQS consumer that tracks S3 Glacier
// restore lifecycle events and writes them onto the files index.
//
// S3 emits ObjectRestore:Post (restoration started) and
// ObjectRestore:Completed (restored copy is available, with an expiry
// time) events. They flow through SNS → SQS as a JSON envelope; this
// package polls the queue, parses each notification, and updates the
// matching db.File row by S3 key.
package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/Wlczak/aws-backup/internal/config"
)

const (
	eventRestorePost      = "ObjectRestore:Post"
	eventRestoreCompleted = "ObjectRestore:Completed"
)

// FileRestoreUpdater is the slice of *db.DB the consumer needs.
type FileRestoreUpdater interface {
	MarkRestoreInProgress(ctx context.Context, s3Key string) (int64, error)
	MarkRestored(ctx context.Context, s3Key string, expiresAt time.Time) (int64, error)
}

// sqsAPI lets tests substitute the AWS client.
type sqsAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, opts ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

// Consumer is a long-running SQS poller. Construct via New, drive via Run.
type Consumer struct {
	client   sqsAPI
	queueURL string
	waitSec  int32
	visSec   int32
	maxMsgs  int32
	db       FileRestoreUpdater
	logger   *slog.Logger
	// OnDrainComplete fires after each successful DrainAll, with the
	// number of messages processed in that drain. The CLI uses this to
	// kick off a "pending-only" restore-status scan so files locally
	// stuck in_progress (because their completion notification was
	// dropped or arrived before the queue was wired up) get reconciled
	// without waiting for a manual full scan. nil disables the hook.
	OnDrainComplete func(ctx context.Context, processed int)

	// receiveMu serialises a Receive+process cycle between Run and
	// DrainAll so the drain can't see "0 messages" while Run is holding
	// a batch with expired visibility, then fire OnDrainComplete and
	// race the still-in-flight per-record updates. (#185)
	receiveMu sync.Mutex
}

// New builds a Consumer, loading AWS credentials from cfg + the parent
// S3 credentials. Returns nil, nil when QueueURL is empty (feature off).
func New(ctx context.Context, cfg config.SQSConfig, s3cfg config.S3Config, db FileRestoreUpdater, logger *slog.Logger) (*Consumer, error) {
	if cfg.QueueURL == "" {
		return nil, nil
	}
	region := cfg.Region
	if region == "" {
		region = s3cfg.Region
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if s3cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(s3cfg.AccessKeyID, s3cfg.SecretAccessKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		o.Region = region
	})

	wait := int32(cfg.WaitTimeSeconds)
	if wait <= 0 {
		wait = 20
	}
	vis := int32(cfg.VisibilityTimeout)
	if vis <= 0 {
		vis = 60
	}
	mx := int32(cfg.MaxMessages)
	if mx <= 0 {
		mx = 10
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{
		client:   client,
		queueURL: cfg.QueueURL,
		waitSec:  wait,
		visSec:   vis,
		maxMsgs:  mx,
		db:       db,
		logger:   logger,
	}, nil
}

// DrainAll short-polls the queue repeatedly until an empty receive comes
// back, applying every message through the same handleMessage path the
// background loop uses. Returns the count of messages processed. Safe to
// call concurrently with Run — the SQS visibility timeout keeps a message
// from being seen by both at once. Used by the "Sync restore status"
// button so users don't have to wait out the long-poll cycle after a
// Glacier restore lands.
func (c *Consumer) DrainAll(ctx context.Context) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		c.receiveMu.Lock()
		out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: c.maxMsgs,
			WaitTimeSeconds:     0, // short-poll: return immediately
			VisibilityTimeout:   c.visSec,
		})
		if err != nil {
			c.receiveMu.Unlock()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return total, err
			}
			return total, fmt.Errorf("sqs receive: %w", err)
		}
		if len(out.Messages) == 0 {
			c.receiveMu.Unlock()
			if c.OnDrainComplete != nil {
				c.OnDrainComplete(ctx, total)
			}
			return total, nil
		}
		for _, msg := range out.Messages {
			c.handleMessage(ctx, msg)
		}
		c.receiveMu.Unlock()
		total += len(out.Messages)
	}
}

// Run polls the queue until ctx is cancelled. Always returns nil on
// clean shutdown (context cancelled); transport errors are logged and
// retried.
func (c *Consumer) Run(ctx context.Context) error {
	c.logger.Info("sqs restore consumer started", "queue", c.queueURL)
	for {
		if err := ctx.Err(); err != nil {
			c.logger.Info("sqs restore consumer stopped")
			return nil
		}
		c.receiveMu.Lock()
		out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: c.maxMsgs,
			WaitTimeSeconds:     c.waitSec,
			VisibilityTimeout:   c.visSec,
		})
		if err != nil {
			c.receiveMu.Unlock()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			c.logger.Error("sqs receive failed", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, msg := range out.Messages {
			c.handleMessage(ctx, msg)
		}
		c.receiveMu.Unlock()
	}
}

func (c *Consumer) handleMessage(ctx context.Context, msg sqstypes.Message) {
	// Per-message heartbeat: a slow batch (busy SQLite, multi-record
	// messages) can outlive the configured visibility timeout, so SQS
	// redelivers the message and the same record is applied twice.
	// Extend visibility every visSec/3 while we're still processing. (#186)
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	if msg.ReceiptHandle != nil && c.visSec > 1 {
		go c.heartbeat(hbCtx, *msg.ReceiptHandle)
	}
	body := ""
	if msg.Body != nil {
		body = *msg.Body
	}
	records, skip, err := parseSNSEnvelope(body)
	if err != nil {
		c.logger.Warn("sqs message parse failed", "err", err)
		// Malformed messages will keep redelivering; rely on the queue's
		// redrive policy / DLQ to drain them. Don't delete blindly.
		return
	}
	// SNS control envelopes (SubscriptionConfirmation,
	// UnsubscribeConfirmation) carry no S3 records but must still be
	// deleted, otherwise they redeliver every visibility-timeout interval
	// and accumulate forever. (#146)
	if skip {
		c.logger.Info("sqs control message acknowledged")
	}
	allOK := true
	for _, rec := range records {
		if err := c.applyRecord(ctx, rec); err != nil {
			c.logger.Error("apply restore record failed", "err", err, "key", rec.Key, "event", rec.EventName)
			allOK = false
		}
	}
	if !allOK {
		return
	}
	if _, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: msg.ReceiptHandle,
	}); err != nil {
		c.logger.Error("sqs delete failed", "err", err)
	}
}

// heartbeat extends a single message's visibility timeout periodically
// until the parent ctx is cancelled (which handleMessage does via defer
// once it's done with the message). (#186)
func (c *Consumer) heartbeat(ctx context.Context, receipt string) {
	interval := time.Duration(c.visSec) * time.Second / 3
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
				QueueUrl:          aws.String(c.queueURL),
				ReceiptHandle:     aws.String(receipt),
				VisibilityTimeout: c.visSec,
			})
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				c.logger.Debug("sqs visibility heartbeat failed", "err", err)
				return
			}
		}
	}
}

func (c *Consumer) applyRecord(ctx context.Context, rec restoreRecord) error {
	switch rec.EventName {
	case eventRestorePost:
		n, err := c.db.MarkRestoreInProgress(ctx, rec.Key)
		if err != nil {
			return err
		}
		if n == 0 {
			c.logger.Info("restore in_progress event for unknown s3_key", "key", rec.Key)
		} else {
			c.logger.Info("restore in_progress", "key", rec.Key, "rows", n)
		}
		return nil
	case eventRestoreCompleted:
		if rec.ExpiresAt.IsZero() {
			return fmt.Errorf("completed event missing lifecycleRestorationExpiryTime")
		}
		n, err := c.db.MarkRestored(ctx, rec.Key, rec.ExpiresAt)
		if err != nil {
			return err
		}
		if n == 0 {
			c.logger.Info("restore completed event for unknown s3_key", "key", rec.Key)
		} else {
			c.logger.Info("restore completed", "key", rec.Key, "expires", rec.ExpiresAt, "rows", n)
		}
		return nil
	default:
		c.logger.Debug("ignoring non-restore event", "event", rec.EventName, "key", rec.Key)
		return nil
	}
}

// restoreRecord is the normalised view of one S3 event record.
type restoreRecord struct {
	EventName string
	Key       string
	ExpiresAt time.Time
}

// parseSNSEnvelope unwraps the SNS notification, decodes the inner S3
// event, and returns one restoreRecord per Records[] entry. The skip
// return is true when the envelope is a benign SNS control message
// (SubscriptionConfirmation / UnsubscribeConfirmation) — the caller
// should delete it from the queue without applying any records. (#146)
func parseSNSEnvelope(body string) (records []restoreRecord, skip bool, err error) {
	if body == "" {
		return nil, false, errors.New("empty body")
	}
	var env struct {
		Type    string `json:"Type"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return nil, false, fmt.Errorf("sns envelope: %w", err)
	}
	switch env.Type {
	case "SubscriptionConfirmation", "UnsubscribeConfirmation":
		// Control messages: drop them silently so they don't redeliver.
		return nil, true, nil
	}
	// SNS Notification messages have Type="Notification". If the field
	// is missing, assume the queue is wired directly to S3 (no SNS hop)
	// and treat the whole body as the S3 event payload.
	inner := env.Message
	if env.Type == "" {
		inner = body
	}
	recs, err := parseS3EventPayload(inner)
	return recs, false, err
}

func parseS3EventPayload(payload string) ([]restoreRecord, error) {
	if payload == "" {
		return nil, errors.New("empty s3 event payload")
	}
	var ev struct {
		Records []struct {
			EventName string `json:"eventName"`
			S3        struct {
				Object struct {
					Key string `json:"key"`
				} `json:"object"`
			} `json:"s3"`
			GlacierEventData *struct {
				RestoreEventData struct {
					LifecycleRestorationExpiryTime string `json:"lifecycleRestorationExpiryTime"`
				} `json:"restoreEventData"`
			} `json:"glacierEventData,omitempty"`
		} `json:"Records"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil, fmt.Errorf("s3 event: %w", err)
	}
	out := make([]restoreRecord, 0, len(ev.Records))
	for _, r := range ev.Records {
		// PathUnescape (not QueryUnescape) so a literal '+' in a real S3
		// key isn't decoded to a space — S3 keys are URL path components,
		// not form fields. (#249)
		key, err := url.PathUnescape(r.S3.Object.Key)
		if err != nil {
			key = r.S3.Object.Key
		}
		rec := restoreRecord{EventName: r.EventName, Key: key}
		if r.GlacierEventData != nil {
			if s := r.GlacierEventData.RestoreEventData.LifecycleRestorationExpiryTime; s != "" {
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					rec.ExpiresAt = t
				} else if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					rec.ExpiresAt = t
				}
			}
		}
		out = append(out, rec)
	}
	return out, nil
}
