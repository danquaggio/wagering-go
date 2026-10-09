"""Idempotent local SQS/IAM fixture. No real AWS credentials."""
import configparser, json, os
from pathlib import Path
import boto3
endpoint = os.environ["AWS_ENDPOINT_URL"]
session = boto3.Session(region_name="us-east-1")
sqs = session.client("sqs", endpoint_url=endpoint)
iam = session.client("iam", endpoint_url=endpoint)
def queue(name, extra=None):
    attrs = {"FifoQueue": "true", "ContentBasedDeduplication": "false",
             "VisibilityTimeout": "30", "MessageRetentionPeriod": "1209600"}
    attrs.update(extra or {})
    url = sqs.create_queue(QueueName=name, Attributes=attrs)["QueueUrl"]
    arn = sqs.get_queue_attributes(QueueUrl=url, AttributeNames=["QueueArn"])["Attributes"]["QueueArn"]
    return url, arn
dlq_url, dlq_arn = queue("wager-transactions-dlq.fifo")
input_url, input_arn = queue("wager-transactions.fifo", {
    "RedrivePolicy": json.dumps({"deadLetterTargetArn": dlq_arn, "maxReceiveCount": "5"})})
output_url, output_arn = queue("wager-events.fifo")
sqs.set_queue_attributes(QueueUrl=dlq_url, Attributes={
    "RedriveAllowPolicy": json.dumps({"redrivePermission": "byQueue", "sourceQueueArns": [input_arn]})})
def statement(actions, resources):
    return {"Effect": "Allow", "Action": actions, "Resource": resources}
profiles = {
    "app": [
        statement(["sqs:GetQueueUrl", "sqs:GetQueueAttributes"], [input_arn, output_arn, dlq_arn]),
        statement(["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility"], [input_arn]),
        statement(["sqs:SendMessage"], [output_arn])],
    "provider-a": [statement(["sqs:GetQueueUrl", "sqs:SendMessage"], [input_arn])],
    "provider-b": [statement(["sqs:GetQueueUrl", "sqs:SendMessage"], [input_arn])],
    "event-reader": [statement(["sqs:GetQueueUrl", "sqs:GetQueueAttributes", "sqs:ReceiveMessage", "sqs:DeleteMessage"], [output_arn, dlq_arn])],
    "denied": [],
}
path = Path("/run/aws/credentials")
creds = configparser.RawConfigParser()
if path.exists():
    creds.read(path)
for profile, statements in profiles.items():
    name = "wager-" + profile
    try:
        iam.create_user(UserName=name)
    except iam.exceptions.EntityAlreadyExistsException:
        pass
    policy = {"Version": "2012-10-17", "Statement": statements or [
        {"Effect": "Deny", "Action": "sqs:*", "Resource": "*"}]}
    iam.put_user_policy(UserName=name, PolicyName="wager-policy", PolicyDocument=json.dumps(policy))
    existing = iam.list_access_keys(UserName=name)["AccessKeyMetadata"]
    saved = creds.get(profile, "aws_access_key_id", fallback=None)
    if not any(key["AccessKeyId"] == saved for key in existing):
        for key in existing:
            iam.delete_access_key(UserName=name, AccessKeyId=key["AccessKeyId"])
        key = iam.create_access_key(UserName=name)["AccessKey"]
        creds[profile] = {"aws_access_key_id": key["AccessKeyId"],
                         "aws_secret_access_key": key["SecretAccessKey"]}
path.parent.mkdir(parents=True, exist_ok=True)
with path.open("w") as file:
    creds.write(file)
os.chmod(path, 0o644)
print("SQS queues, redrive and five IAM profiles provisioned.")
