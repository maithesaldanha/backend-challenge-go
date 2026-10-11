package awssqs

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/application/events"
)

func TestLocalSQSMessageRoundTrip(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("TEST_SQS_ENDPOINT_URL"))
	if endpoint == "" {
		t.Skip("TEST_SQS_ENDPOINT_URL is not set")
	}
	parsedEndpoint, err := url.ParseRequestURI(endpoint)
	if err != nil || parsedEndpoint.Scheme != "http" || !isLocalSQSEndpoint(parsedEndpoint.Hostname()) {
		t.Fatal("TEST_SQS_ENDPOINT_URL must use HTTP and a localhost endpoint")
	}
	region := strings.TrimSpace(os.Getenv("TEST_SQS_REGION"))
	if region == "" {
		region = "us-east-1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", region)

	config, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	api := sqs.NewFromConfig(config, func(options *sqs.Options) {
		options.BaseEndpoint = aws.String(endpoint)
	})
	queueName := "backend-challenge-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".fifo"
	created, err := api.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(queueName),
		Attributes: map[string]string{
			string(types.QueueAttributeNameFifoQueue):                  "true",
			string(types.QueueAttributeNameContentBasedDeduplication): "false",
			string(types.QueueAttributeNameVisibilityTimeout):         "60",
		},
	})
	if err != nil {
		t.Fatalf("create isolated SQS queue: %v", err)
	}
	queueURL := aws.ToString(created.QueueUrl)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = api.DeleteQueue(cleanupContext, &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)})
	})
	client, err := New(Config{Region: region, Endpoint: endpoint, QueueURL: queueURL, InputQueueURL: queueURL, DLQQueueURL: queueURL})
	if err != nil {
		t.Fatal(err)
	}
	event := events.Event{
		ID: uuid.NewString(), AggregateID: uuid.NewString(), Type: "IntegrationTestEvent",
		CorrelationID: uuid.NewString(), OccurredAt: time.Now().UTC(), Version: 1, Payload: json.RawMessage(`{"value":"round-trip"}`),
	}
	if err := client.Publish(ctx, event); err != nil {
		t.Fatalf("publish event to local SQS: %v", err)
	}
	messages, err := client.Receive(ctx)
	if err != nil {
		t.Fatalf("receive event from local SQS: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("received %d messages, want 1", len(messages))
	}
	var envelope struct {
		EventID     string `json:"eventId"`
		AggregateID string `json:"aggregateId"`
		EventType   string `json:"eventType"`
	}
	if err := json.Unmarshal(messages[0].Body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.EventID != event.ID || envelope.AggregateID != event.AggregateID || envelope.EventType != event.Type {
		t.Fatalf("received envelope = %#v", envelope)
	}
	if err := client.Delete(ctx, messages[0].ReceiptHandle); err != nil {
		t.Fatalf("delete received local SQS message: %v", err)
	}
}

func isLocalSQSEndpoint(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "localstack"
}
