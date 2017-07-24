package producer

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/aws/aws-sdk-go/service/kinesis/kinesisiface"

	"github.com/rewardStyle/kinetic/errs"
	"github.com/rewardStyle/kinetic/logging"
	"github.com/rewardStyle/kinetic/message"
)

type kinesisWriterOptions struct {
	responseReadTimeout time.Duration  // maximum time to wait for PutRecords API call before timing out
	msgCountRateLimit   int            // maximum number of records to be sent per second
	msgSizeRateLimit    int            // maximum (transmission) size of records to be sent per second
	Stats               StatsCollector // stats collection mechanism
}

// KinesisWriter handles the API to send records to Kinesis.
type KinesisWriter struct {
	*kinesisWriterOptions
	*logging.LogHelper

	stream string
	client kinesisiface.KinesisAPI
}

// NewKinesisWriter creates a new stream writer to write records to a Kinesis.
func NewKinesisWriter(c *aws.Config, stream string, fn ...func(*KinesisWriterConfig)) (*KinesisWriter, error) {
	cfg := NewKinesisWriterConfig(c)
	for _, f := range fn {
		f(cfg)
	}
	sess, err := session.NewSession(cfg.AwsConfig)
	if err != nil {
		return nil, err
	}
	return &KinesisWriter{
		kinesisWriterOptions: cfg.kinesisWriterOptions,
		LogHelper: &logging.LogHelper{
			LogLevel: cfg.LogLevel,
			Logger:   cfg.AwsConfig.Logger,
		},
		stream: stream,
		client: kinesis.New(sess),
	}, nil
}

// PutRecords sends a batch of records to Kinesis and returns a list of records that need to be retried.
func (w *KinesisWriter) PutRecords(ctx context.Context, messages []*message.Message, fn MessageHandlerAsync) error {
	var startSendTime time.Time
	var startBuildTime time.Time

	start := time.Now()
	var records []*kinesis.PutRecordsRequestEntry
	for _, msg := range messages {
		if msg != nil {
			records = append(records, msg.ToRequestEntry())
		}
	}
	req, resp := w.client.PutRecordsRequest(&kinesis.PutRecordsInput{
		StreamName: aws.String(w.stream),
		Records:    records,
	})
	req.ApplyOptions(request.WithResponseReadTimeout(w.responseReadTimeout))

	req.Handlers.Build.PushFront(func(r *request.Request) {
		startBuildTime = time.Now()
		w.LogDebug("Start PutRecords Build, took", time.Since(start))
	})

	req.Handlers.Build.PushBack(func(r *request.Request) {
		w.Stats.AddPutRecordsBuildDuration(time.Since(startBuildTime))
		w.LogDebug("Finished PutRecords Build, took", time.Since(start))
	})

	req.Handlers.Send.PushFront(func(r *request.Request) {
		startSendTime = time.Now()
		w.LogDebug("Start PutRecords Send took", time.Since(start))
	})

	req.Handlers.Send.PushBack(func(r *request.Request) {
		w.Stats.AddPutRecordsSendDuration(time.Since(startSendTime))
		w.LogDebug("Finished PutRecords Send, took", time.Since(start))
	})

	w.LogDebug("Starting PutRecords Build/Sign request, took", time.Since(start))
	w.Stats.AddPutRecordsCalled(1)
	if err := req.Send(); err != nil {
		w.LogError("Error putting records:", err.Error())
		return err
	}
	w.Stats.AddPutRecordsDuration(time.Since(start))

	if resp == nil {
		return errs.ErrNilPutRecordsResponse
	}
	if resp.FailedRecordCount == nil {
		return errs.ErrNilFailedRecordCount
	}
	attempted := len(messages)
	failed := int(aws.Int64Value(resp.FailedRecordCount))
	sent := attempted - failed
	w.LogDebug(fmt.Sprintf("Finished PutRecords request, %d records attempted, %d records successful, %d records failed, took %v\n", attempted, sent, failed, time.Since(start)))

	for idx, record := range resp.Records {
		if record.SequenceNumber != nil && record.ShardId != nil {
			// TODO: per-shard metrics
			messages[idx].SequenceNumber = record.SequenceNumber
			messages[idx].ShardID = record.ShardId
			w.Stats.AddSentSuccess(1)
		} else {
			switch aws.StringValue(record.ErrorCode) {
			case kinesis.ErrCodeProvisionedThroughputExceededException:
				w.Stats.AddProvisionedThroughputExceeded(1)
			default:
				w.LogDebug("PutRecords record failed with error:", aws.StringValue(record.ErrorCode), aws.StringValue(record.ErrorMessage))
			}
			messages[idx].ErrorCode = record.ErrorCode
			messages[idx].ErrorMessage = record.ErrorMessage
			messages[idx].FailCount++
			w.Stats.AddSentFailed(1)

			fn(messages[idx])
		}
	}

	return nil
}

// getMsgCountRateLimit returns the writer's message count rate limit
func (w *KinesisWriter) getMsgCountRateLimit() int {
	return w.msgCountRateLimit
}

// getMsgSizeRateLimit returns the writer's message size rate limit
func (w *KinesisWriter) getMsgSizeRateLimit() int {
	return w.msgSizeRateLimit
}

// getConcurrencyMultiplier returns the writer's concurrency multiplier.  For the kinesiswriter the multiplier is the
// number of active shards for the Kinesis stream
func (w *KinesisWriter) getConcurrencyMultiplier() (int, error) {
	resp, err := w.client.DescribeStream(&kinesis.DescribeStreamInput{
		StreamName: aws.String(w.stream),
	})
	if err != nil {
		w.LogError("Error describing kinesis stream: ", w.stream, err)
		return 0, err
	}
	if resp == nil {
		return 0, errs.ErrNilDescribeStreamResponse
	}
	if resp.StreamDescription == nil {
		return 0, errs.ErrNilStreamDescription
	}
	var shards []string
	for _, shard := range resp.StreamDescription.Shards {
		if shard.ShardId != nil {
			shards = append(shards, aws.StringValue(shard.ShardId))
		}
	}
	return len(shards), nil
}
