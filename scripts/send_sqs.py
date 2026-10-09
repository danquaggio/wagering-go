"""Requires boto3, already present in the Compose producer image."""
import argparse, hashlib, hmac, json, os, uuid
from datetime import datetime, timezone
import boto3
p = argparse.ArgumentParser()
p.add_argument("--wallet", required=True)
p.add_argument("--player", required=True)
p.add_argument("--external", default=None)
p.add_argument("--message", default=None)
p.add_argument("--provider", choices=["provider-a", "provider-b"], default="provider-a")
p.add_argument("--kind", choices=["BET", "WIN", "LOSS", "REFUND", "ROLLBACK"], default="BET")
p.add_argument("--amount", default="25.00")
p.add_argument("--round", default="round-demo")
p.add_argument("--key", default=None)
p.add_argument("--reference", default=None)
p.add_argument("--repeat", type=int, default=1)
a = p.parse_args()
external = a.external or str(uuid.uuid4())
data = {"providerId": a.provider, "externalTransactionId": external,
        "idempotencyKey": a.key or external, "walletId": a.wallet, "playerId": a.player,
        "roundId": a.round, "gameId": "game-demo", "kind": a.kind,
        "money": {"amount": a.amount, "currency": "BRL"}}
if a.reference: data["referenceExternalTransactionId"] = a.reference
envelope = {"messageId": a.message or str(uuid.uuid4()), "type": "WagerTransactionRequested",
            "occurredAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"), "data": data}
raw = json.dumps(envelope, separators=(",", ":"))
keys = {"provider-a": "local-provider-a-signing-key-00000001",
        "provider-b": "local-provider-b-signing-key-00000002"}
signature = hmac.new(keys[a.provider].encode(), raw.encode(), hashlib.sha256).hexdigest()
session = boto3.Session(profile_name=a.provider, region_name="us-east-1")
sqs = session.client("sqs", endpoint_url=os.getenv("SQS_ENDPOINT", "http://localhost:4566"))
queue = sqs.get_queue_url(QueueName="wager-transactions.fifo")["QueueUrl"]
for _ in range(a.repeat):
    sqs.send_message(QueueUrl=queue, MessageBody=raw, MessageGroupId=a.wallet,
                     MessageDeduplicationId=str(uuid.uuid4()),
                     MessageAttributes={"providerSignature": {"DataType": "String", "StringValue": signature}})
print(json.dumps({"messageId": envelope["messageId"], "externalTransactionId": external,
                  "deliveriesRequested": a.repeat}))
