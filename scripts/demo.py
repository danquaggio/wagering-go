"""Run on Windows with Python, or through the Docker Compose demo service."""
import json, os, time, uuid, urllib.request, urllib.parse, urllib.error

BASE = os.getenv("API_URL", "http://localhost:8080")
TOKEN_URL = os.getenv("TOKEN_URL", "http://localhost:8081/realms/wagering/protocol/openid-connect/token")
def access_token(client):
    body = urllib.parse.urlencode({"grant_type": "client_credentials", "client_id": client,
                                  "client_secret": client + "-local-secret"}).encode()
    with urllib.request.urlopen(TOKEN_URL, body, timeout=10) as response:
        return json.load(response)["access_token"]
def api(method, path, client, data=None, key=None, base=None):
    headers = {"Authorization": "Bearer " + access_token(client), "Content-Type": "application/json"}
    if key: headers["Idempotency-Key"] = key
    request = urllib.request.Request((base or BASE) + path, method=method, headers=headers,
                                     data=json.dumps(data).encode() if data is not None else None)
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)
def ready(base=None):
    until = time.monotonic() + 90
    while time.monotonic() < until:
        try:
            with urllib.request.urlopen((base or BASE)+"/health/ready", timeout=4) as response:
                if response.status == 200: return
        except (OSError, urllib.error.HTTPError):
            pass
        time.sleep(0.5)
    raise RuntimeError("A aplicação não ficou pronta.")
def wallet(initial="100.00", base=None):
    code, data = api("POST", "/wallets", "wallet-service",
                     {"playerId": str(uuid.uuid4()), "initialBalance": {"amount": initial, "currency": "BRL"}}, base=base)
    if code != 201: raise RuntimeError((code, data))
    return data
def operation(w, kind="BET", value="25.00"):
    external = str(uuid.uuid4())
    return {"providerId": "provider-a", "externalTransactionId": external,
            "playerId": w["playerId"], "walletId": w["id"], "roundId": "round-demo",
            "gameId": "game-demo", "kind": kind, "money": {"amount": value, "currency": "BRL"}}
def main():
    ready()
    w = wallet()
    print("Carteira criada:", json.dumps(w, ensure_ascii=False))
    data = operation(w)
    for label in ("Aposta", "Replay da mesma aposta"):
        code, result = api("POST", "/wagering/transactions", "provider-a", data, data["externalTransactionId"])
        print(label, code, json.dumps(result, ensure_ascii=False))
    code, result = api("POST", "/wallets/"+w["id"]+"/reconciliation", "wallet-service")
    print("Reconciliação", code, json.dumps(result, ensure_ascii=False))
    assert result["consistent"] and result["storedBalance"]["amount"] == "75.00"
if __name__ == "__main__":
    main()
