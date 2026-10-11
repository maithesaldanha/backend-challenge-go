package awssqs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/junglegaming/backend-challenge-go/internal/application/events"
)

var ErrInvalidConfig = errors.New("invalid SQS configuration")

type Config struct {
	Region   string
	Endpoint string
	QueueURL string
}

type Client struct {
	api      sqsAPI
	queueURL string
}

type sqsAPI interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
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
	if strings.TrimSpace(config.Region) == "" || strings.TrimSpace(config.QueueURL) == "" {
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
	return &Client{api: client, queueURL: strings.TrimSpace(config.QueueURL)}, nil
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
	_, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(c.queueURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	return err
}
