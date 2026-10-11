package awssqs

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/junglegaming/backend-challenge-go/internal/application/events"
)

var ErrInvalidConfig = errors.New("invalid SQS configuration")

type Config struct {
	Region        string
	Endpoint      string
	QueueURL      string
	InputQueueURL string
}

type Client struct {
	api           sqsAPI
	queueURL      string
	inputQueueURL string
}

type sqsAPI interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

type ReceivedMessage struct {
	ID            string
	ReceiptHandle string
	Body          []byte
	ReceiveCount  int
}

type envelope struct {
	EventID       string          `json:"eventId"`
	EventType     string          `json:"eventType"`
	AggregateID   string          `json:"aggregateId"`
	CorrelationID string          `json:"correlationId"`
	CausationID   string          `json:"causationId,omitempty"`
	OccurredAt    string          `json:"occurredAt"`
	Version       int             `json:"version"`
	Data          json.RawMessage `json:"data"`
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.Region) == "" || strings.TrimSpace(config.QueueURL) == "" || strings.TrimSpace(config.InputQueueURL) == "" {
		return nil, ErrInvalidConfig
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(config.Region))
	if err != nil {
		return nil, err
	}
	client := sqs.NewFromConfig(awsConfig, func(options *sqs.Options) {
		if strings.TrimSpace(config.Endpoint) != "" {
			options.BaseEndpoint = aws.String(strings.TrimSpace(config.Endpoint))
		}
	})
	return &Client{api: client, queueURL: strings.TrimSpace(config.QueueURL), inputQueueURL: strings.TrimSpace(config.InputQueueURL)}, nil
}

func (c *Client) Publish(ctx context.Context, event events.Event) error {
	message, err := json.Marshal(envelope{
		EventID:       event.ID,
		EventType:     event.Type,
		AggregateID:   event.AggregateID,
		CorrelationID: event.CorrelationID,
		CausationID:   event.CausationID,
		OccurredAt:    event.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Version:       event.Version,
		Data:          event.Payload,
	})
	if err != nil {
		return err
	}
	_, err = c.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.queueURL),
		MessageBody:            aws.String(string(message)),
		MessageGroupId:         aws.String(event.AggregateID),
		MessageDeduplicationId: aws.String(event.ID),
	})
	return err
}

func (c *Client) Name() string { return "sqs" }

func (c *Client) Check(ctx context.Context) error {
	for _, queueURL := range []string{c.queueURL, c.inputQueueURL} {
		_, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(queueURL),
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Receive(ctx context.Context) ([]ReceivedMessage, error) {
	output, err := c.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(c.inputQueueURL),
		MaxNumberOfMessages:         1,
		WaitTimeSeconds:             20,
		VisibilityTimeout:           60,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, err
	}
	messages := make([]ReceivedMessage, 0, len(output.Messages))
	for _, message := range output.Messages {
		if message.ReceiptHandle == nil || message.Body == nil {
			continue
		}
		id := ""
		if message.MessageId != nil {
			id = *message.MessageId
		}
		receiveCount := 1
		if value, ok := message.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]; ok {
			if parsed, parseErr := strconv.Atoi(value); parseErr == nil && parsed > 0 {
				receiveCount = parsed
			}
		}
		messages = append(messages, ReceivedMessage{ID: id, ReceiptHandle: *message.ReceiptHandle, Body: []byte(*message.Body), ReceiveCount: receiveCount})
	}
	return messages, nil
}

func (c *Client) Delete(ctx context.Context, receiptHandle string) error {
	_, err := c.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.inputQueueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	return err
}

func (c *Client) Retry(ctx context.Context, receiptHandle string, receiveCount int) error {
	if receiveCount < 1 {
		receiveCount = 1
	}
	if receiveCount > 6 {
		receiveCount = 6
	}
	visibilitySeconds := 1 << uint(receiveCount-1)
	_, err := c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.inputQueueURL),
		ReceiptHandle:     aws.String(receiptHandle),
		VisibilityTimeout: int32(visibilitySeconds),
	})
	return err
}
