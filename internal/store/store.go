// Package store persists subscriptions and charge records in DynamoDB.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Status string

const (
	StatusPending        Status = "PENDING"
	StatusSucceeded      Status = "SUCCEEDED"
	StatusFailed         Status = "FAILED"          // retryable
	StatusRequiresAction Status = "REQUIRES_ACTION" // 3DS etc. needs the customer
	StatusAbandoned      Status = "ABANDONED"       // retries exhausted
)

type Subscription struct {
	CustomerID      string `dynamodbav:"customer_id"`
	PaymentMethodID string `dynamodbav:"payment_method_id"`
	Amount          int64  `dynamodbav:"amount"`
	Currency        string `dynamodbav:"currency"`
	Active          bool   `dynamodbav:"active"`
}

// Charge is one billing attempt series for (customer, period). ChargeID = customer#period.
type Charge struct {
	ChargeID        string `dynamodbav:"charge_id"`
	CustomerID      string `dynamodbav:"customer_id"`
	Period          string `dynamodbav:"period"` // e.g. 2026-10
	PaymentMethodID string `dynamodbav:"payment_method_id"`
	Amount          int64  `dynamodbav:"amount"`
	Currency        string `dynamodbav:"currency"`
	Status          Status `dynamodbav:"status"`
	Attempts        int    `dynamodbav:"attempts"`
	PaymentIntentID string `dynamodbav:"payment_intent_id,omitempty"`
	LastError       string `dynamodbav:"last_error,omitempty"`
	NextRetryAt     int64  `dynamodbav:"next_retry_at,omitempty"` // unix seconds
	UpdatedAt       int64  `dynamodbav:"updated_at"`
}

var ErrExists = errors.New("charge already exists")

type Store struct {
	db                 *dynamodb.Client
	subsTable, chTable string
}

func New(db *dynamodb.Client, subsTable, chargesTable string) *Store {
	return &Store{db: db, subsTable: subsTable, chTable: chargesTable}
}

func ChargeID(customerID, period string) string { return customerID + "#" + period }

func (s *Store) ActiveSubscriptions(ctx context.Context) ([]Subscription, error) {
	var out []Subscription
	p := dynamodb.NewScanPaginator(s.db, &dynamodb.ScanInput{
		TableName:                 aws.String(s.subsTable),
		FilterExpression:          aws.String("active = :t"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": &types.AttributeValueMemberBOOL{Value: true}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		var items []Subscription
		if err := attributevalue.UnmarshalListOfMaps(page.Items, &items); err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func (s *Store) PutSubscription(ctx context.Context, sub Subscription) error {
	item, err := attributevalue.MarshalMap(sub)
	if err != nil {
		return err
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(s.subsTable), Item: item})
	return err
}

// CreateCharge inserts a PENDING charge; returns ErrExists if already created for the period
// (this makes the monthly job safe to re-run).
func (s *Store) CreateCharge(ctx context.Context, c Charge) error {
	c.Status, c.UpdatedAt = StatusPending, time.Now().Unix()
	item, err := attributevalue.MarshalMap(c)
	if err != nil {
		return err
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.chTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(charge_id)"),
	})
	var cce *types.ConditionalCheckFailedException
	if errors.As(err, &cce) {
		return ErrExists
	}
	return err
}

func (s *Store) UpdateCharge(ctx context.Context, c Charge) error {
	c.UpdatedAt = time.Now().Unix()
	item, err := attributevalue.MarshalMap(c)
	if err != nil {
		return err
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(s.chTable), Item: item})
	return err
}

func (s *Store) GetCharge(ctx context.Context, id string) (*Charge, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.chTable),
		Key:       map[string]types.AttributeValue{"charge_id": &types.AttributeValueMemberS{Value: id}},
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, fmt.Errorf("charge %s not found", id)
	}
	var c Charge
	return &c, attributevalue.UnmarshalMap(out.Item, &c)
}

// ChargesByStatus scans charges with the given status.
func (s *Store) ChargesByStatus(ctx context.Context, st Status) ([]Charge, error) {
	var out []Charge
	p := dynamodb.NewScanPaginator(s.db, &dynamodb.ScanInput{
		TableName:                 aws.String(s.chTable),
		FilterExpression:          aws.String("#s = :s"),
		ExpressionAttributeNames:  map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":s": &types.AttributeValueMemberS{Value: string(st)}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		var items []Charge
		if err := attributevalue.UnmarshalListOfMaps(page.Items, &items); err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// EnsureTables creates both tables if missing (local dev; Terraform owns them in AWS).
func (s *Store) EnsureTables(ctx context.Context) error {
	for _, t := range []struct{ name, key string }{{s.subsTable, "customer_id"}, {s.chTable, "charge_id"}} {
		_, err := s.db.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName:            aws.String(t.name),
			BillingMode:          types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String(t.key), AttributeType: types.ScalarAttributeTypeS}},
			KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String(t.key), KeyType: types.KeyTypeHash}},
		})
		var inUse *types.ResourceInUseException
		if err != nil && !errors.As(err, &inUse) {
			return err
		}
	}
	return nil
}
