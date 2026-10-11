package awssqs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/junglegaming/backend-challenge-go/internal/application/events"
)

type sqsAPIFake struct {
	sent       *sqs.SendMessageInput
	visibility *sqs.ChangeMessageVisibilityInput
	deleted    *sqs.DeleteMessageInput
	received   *sqs.ReceiveMessageInput
	attributes *sqs.GetQueueAttributesInput
}

func (f *sqsAPIFake) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.sent = input
	return &sqs.SendMessageOutput{}, nil
}

func (f *sqsAPIFake) GetQueueAttributes(_ context.Context, input *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	f.attributes = input
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{string(types.QueueAttributeNameApproximateNumberOfMessages): "7"}}, nil
}

func (f *sqsAPIFake) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.received = input
	messageID, receipt, body, count := "sqs-message", "receipt", `{"messageId":"envelope-message"}`, "5"
	return &sqs.ReceiveMessageOutput{Messages: []types.Message{{
		MessageId: &messageID, ReceiptHandle: &receipt, Body: &body,
		Attributes: map[string]string{string(types.MessageSystemAttributeNameApproximateReceiveCount): count},
	}}}, nil
}

func (f *sqsAPIFake) DeleteMessage(_ context.Context, input *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deleted = input
	return &sqs.DeleteMessageOutput{}, nil
}

func (f *sqsAPIFake) ChangeMessageVisibility(_ context.Context, input *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.visibility = input
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func TestPublishUsesStableFIFOIdentityAndTypedEnvelope(t *testing.T) {
	api := &sqsAPIFake{}
	client := &Client{api: api, queueURL: "events.fifo", inputQueueURL: "input.fifo", dlqQueueURL: "dead.fifo"}
	event := events.Event{
		ID: "event-1", Type: "WagerTransactionProcessed", AggregateID: "wallet-1", CorrelationID: "correlation-1",
		OccurredAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), Version: 1, Payload: json.RawMessage(`{"transactionId":"transaction-1"}`),
	}
	if err := client.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(api.sent.QueueUrl) != "events.fifo" || aws.ToString(api.sent.MessageGroupId) != event.AggregateID || aws.ToString(api.sent.MessageDeduplicationId) != event.ID {
		t.Fatalf("FIFO identity = (%q, %q, %q)", aws.ToString(api.sent.QueueUrl), aws.ToString(api.sent.MessageGroupId), aws.ToString(api.sent.MessageDeduplicationId))
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(aws.ToString(api.sent.MessageBody)), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["eventId"] != event.ID || envelope["eventType"] != event.Type || envelope["aggregateId"] != event.AggregateID || envelope["correlationId"] != event.CorrelationID || envelope["version"] != float64(event.Version) {
		t.Fatalf("event envelope = %#v", envelope)
	}
	if envelope["occurredAt"] != "2026-10-11T12:00:00Z" {
		t.Fatalf("occurredAt = %v", envelope["occurredAt"])
	}
}

func TestReceiveDeleteRetryAndReleaseUseInputQueue(t *testing.T) {
	api := &sqsAPIFake{}
	client := &Client{api: api, queueURL: "events.fifo", inputQueueURL: "input.fifo", dlqQueueURL: "dead.fifo"}
	messages, err := client.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(api.received.QueueUrl) != "input.fifo" || api.received.MaxNumberOfMessages != 1 || api.received.VisibilityTimeout != 60 {
		t.Fatalf("receive input = %#v", api.received)
	}
	if len(messages) != 1 || messages[0].ID != "sqs-message" || messages[0].ReceiveCount != 5 || string(messages[0].Body) != `{"messageId":"envelope-message"}` {
		t.Fatalf("received messages = %#v", messages)
	}
	if err := client.Retry(context.Background(), "receipt", 4); err != nil {
		t.Fatal(err)
	}
	if api.visibility.VisibilityTimeout != 8 {
		t.Fatalf("retry visibility = %d, want 8 seconds", api.visibility.VisibilityTimeout)
	}
	if err := client.Release(context.Background(), "receipt"); err != nil {
		t.Fatal(err)
	}
	if api.visibility.VisibilityTimeout != 0 {
		t.Fatalf("release visibility = %d, want 0", api.visibility.VisibilityTimeout)
	}
	if err := client.Delete(context.Background(), "receipt"); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(api.deleted.QueueUrl) != "input.fifo" || aws.ToString(api.deleted.ReceiptHandle) != "receipt" {
		t.Fatalf("delete input = %#v", api.deleted)
	}
	depth, err := client.DLQDepth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if depth != 7 || aws.ToString(api.attributes.QueueUrl) != "dead.fifo" {
		t.Fatalf("DLQ depth = %d for queue %q, want 7 for dead.fifo", depth, aws.ToString(api.attributes.QueueUrl))
	}
}
