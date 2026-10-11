set -eu

endpoint=http://localstack:4566
region=${AWS_DEFAULT_REGION:-us-east-1}

aws --endpoint-url "$endpoint" --region "$region" sqs create-queue \
  --queue-name domain-events.fifo \
  --attributes '{"FifoQueue":"true","ContentBasedDeduplication":"false"}'

aws --endpoint-url "$endpoint" --region "$region" sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes '{"FifoQueue":"true","ContentBasedDeduplication":"false"}'

dlq_url="$endpoint/queue/$region/000000000000/wager-transactions-dlq.fifo"
dlq_arn=$(aws --endpoint-url "$endpoint" --region "$region" sqs get-queue-attributes \
  --queue-url "$dlq_url" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' \
  --output text)

redrive_policy=$(printf '{"deadLetterTargetArn":"%s","maxReceiveCount":"5"}' "$dlq_arn" | sed 's/"/\\"/g')
attributes=$(printf '{"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"60","ReceiveMessageWaitTimeSeconds":"20","RedrivePolicy":"%s"}' "$redrive_policy")

aws --endpoint-url "$endpoint" --region "$region" sqs create-queue \
  --queue-name wager-transactions.fifo \
  --attributes "$attributes"
