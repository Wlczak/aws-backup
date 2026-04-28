package restore

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const samplePostBody = `{
  "Type": "Notification",
  "Message": "{\"Records\":[{\"eventName\":\"ObjectRestore:Post\",\"s3\":{\"object\":{\"key\":\"backups%2Ffile%20one.zip\"}}}]}"
}`

const sampleCompletedBody = `{
  "Type": "Notification",
  "Message": "{\"Records\":[{\"eventName\":\"ObjectRestore:Completed\",\"s3\":{\"object\":{\"key\":\"backups/data.bin\"}},\"glacierEventData\":{\"restoreEventData\":{\"lifecycleRestorationExpiryTime\":\"2026-05-05T12:34:56.000Z\"}}}]}"
}`

const sampleDirectS3Body = `{"Records":[{"eventName":"ObjectRestore:Post","s3":{"object":{"key":"k1"}}}]}`

func TestParseSNSEnvelope_Post(t *testing.T) {
	recs, err := parseSNSEnvelope(samplePostBody)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].EventName != "ObjectRestore:Post" {
		t.Errorf("event = %q", recs[0].EventName)
	}
	if recs[0].Key != "backups/file one.zip" {
		t.Errorf("key = %q (URL-decoding failed)", recs[0].Key)
	}
	if !recs[0].ExpiresAt.IsZero() {
		t.Errorf("Post should not carry expiry, got %v", recs[0].ExpiresAt)
	}
}

func TestParseSNSEnvelope_Completed(t *testing.T) {
	recs, err := parseSNSEnvelope(sampleCompletedBody)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	want := time.Date(2026, 5, 5, 12, 34, 56, 0, time.UTC)
	if !recs[0].ExpiresAt.Equal(want) {
		t.Errorf("expiry = %v, want %v", recs[0].ExpiresAt, want)
	}
	if recs[0].Key != "backups/data.bin" {
		t.Errorf("key = %q", recs[0].Key)
	}
}

func TestParseSNSEnvelope_DirectS3(t *testing.T) {
	recs, err := parseSNSEnvelope(sampleDirectS3Body)
	if err != nil {
		t.Fatalf("parse direct: %v", err)
	}
	if len(recs) != 1 || recs[0].Key != "k1" {
		t.Fatalf("unexpected: %+v", recs)
	}
}

func TestParseSNSEnvelope_Empty(t *testing.T) {
	if _, err := parseSNSEnvelope(""); err == nil {
		t.Fatal("expected error on empty body")
	}
}

func TestParseSNSEnvelope_Garbage(t *testing.T) {
	if _, err := parseSNSEnvelope("not json"); err == nil {
		t.Fatal("expected error on garbage body")
	}
}

// fakeUpdater records calls so handleMessage can be tested end-to-end
// without a real DB.
type fakeUpdater struct {
	inProgress []string
	restored   []struct {
		Key     string
		Expires time.Time
	}
	rows int64
	err  error
}

func (f *fakeUpdater) MarkRestoreInProgress(_ context.Context, key string) (int64, error) {
	f.inProgress = append(f.inProgress, key)
	return f.rows, f.err
}

func (f *fakeUpdater) MarkRestored(_ context.Context, key string, expires time.Time) (int64, error) {
	f.restored = append(f.restored, struct {
		Key     string
		Expires time.Time
	}{key, expires})
	return f.rows, f.err
}

// fakeSQS satisfies sqsAPI for handleMessage's DeleteMessage call.
type fakeSQS struct {
	deleted []string
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	return nil, errors.New("not used")
}
func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	if in.ReceiptHandle != nil {
		f.deleted = append(f.deleted, *in.ReceiptHandle)
	}
	return &sqs.DeleteMessageOutput{}, nil
}

func newTestConsumer(db FileRestoreUpdater, sq *fakeSQS) *Consumer {
	return &Consumer{
		client:   sq,
		queueURL: "https://example/q",
		db:       db,
		logger:   slog.New(slog.NewTextHandler(testWriter{}, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestHandleMessage_Post(t *testing.T) {
	upd := &fakeUpdater{rows: 3}
	sq := &fakeSQS{}
	c := newTestConsumer(upd, sq)
	c.handleMessage(context.Background(), sqstypes.Message{
		Body:          aws.String(samplePostBody),
		ReceiptHandle: aws.String("rh-1"),
	})
	if len(upd.inProgress) != 1 || upd.inProgress[0] != "backups/file one.zip" {
		t.Errorf("inProgress = %v", upd.inProgress)
	}
	if len(sq.deleted) != 1 || sq.deleted[0] != "rh-1" {
		t.Errorf("delete not called: %v", sq.deleted)
	}
}

func TestHandleMessage_Completed(t *testing.T) {
	upd := &fakeUpdater{rows: 1}
	sq := &fakeSQS{}
	c := newTestConsumer(upd, sq)
	c.handleMessage(context.Background(), sqstypes.Message{
		Body:          aws.String(sampleCompletedBody),
		ReceiptHandle: aws.String("rh-2"),
	})
	if len(upd.restored) != 1 {
		t.Fatalf("restored = %v", upd.restored)
	}
	want := time.Date(2026, 5, 5, 12, 34, 56, 0, time.UTC)
	if !upd.restored[0].Expires.Equal(want) {
		t.Errorf("expiry mismatch: %v", upd.restored[0].Expires)
	}
	if len(sq.deleted) != 1 {
		t.Errorf("expected delete after success")
	}
}

func TestHandleMessage_DBErrorKeepsMessage(t *testing.T) {
	upd := &fakeUpdater{err: errors.New("db down")}
	sq := &fakeSQS{}
	c := newTestConsumer(upd, sq)
	c.handleMessage(context.Background(), sqstypes.Message{
		Body:          aws.String(samplePostBody),
		ReceiptHandle: aws.String("rh-3"),
	})
	if len(sq.deleted) != 0 {
		t.Errorf("must not delete on DB error: %v", sq.deleted)
	}
}

func TestHandleMessage_GarbageNotDeleted(t *testing.T) {
	upd := &fakeUpdater{}
	sq := &fakeSQS{}
	c := newTestConsumer(upd, sq)
	c.handleMessage(context.Background(), sqstypes.Message{
		Body:          aws.String("not json"),
		ReceiptHandle: aws.String("rh-4"),
	})
	if len(sq.deleted) != 0 {
		t.Error("malformed message should be left for DLQ, not deleted")
	}
	if len(upd.inProgress) != 0 || len(upd.restored) != 0 {
		t.Error("no DB writes expected for garbage body")
	}
}
