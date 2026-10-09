"""Docker-only failure scenarios. Run in this disposable lab, without another test suite."""
import argparse, json, subprocess, time, uuid
from pathlib import Path
from demo import api, ready, wallet, operation

ROOT = Path(__file__).resolve().parent.parent
APPS = ["app-1", "app-2", "app-3"]
def run(*args, timeout=120, check=True):
    result = subprocess.run(list(args), cwd=ROOT, text=True, capture_output=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError(result.stderr or result.stdout)
    return result.stdout.strip()
def compose(*args, **kwargs):
    return run("docker", "compose", *args, **kwargs)
def sql(statement):
    return compose("exec", "-T", "postgres", "psql", "-U", "postgres", "-d", "wagering", "-XAt", "-v", "ON_ERROR_STOP=1", "-c", statement)
def wait_condition(fn, timeout=70):
    end = time.monotonic()+timeout
    while time.monotonic()<end:
        if fn(): return
        time.sleep(0.5)
    raise RuntimeError("Condição esperada não foi observada.")
def start_victim(point, name, publish_port=False):
    args=["run", "-d", "--no-deps", "--name", name, "-e", "ALLOW_FAILPOINTS=true", "-e", "FAILPOINT="+point]
    if publish_port: args += ["-p", "127.0.0.1:18080:8080"]
    compose(*args, "app-1")
    run("docker","update","--restart=no",name)
    wait_condition(lambda: "application started" in run("docker","logs",name), 90)
def restore(name):
    run("docker","rm","-f",name, check=False)
    compose("start",*APPS)
    ready()
def consumer_crash():
    ready()
    w=wallet()
    # Wait for opening events to be published before isolating this experiment.
    wait_condition(lambda: sql("SELECT count(*) FROM outbox WHERE aggregate_id='"+w["id"]+"' AND published_at IS NULL")=="0")
    compose("stop",*APPS)
    name="wager-consumer-fault-"+uuid.uuid4().hex[:8]
    external=str(uuid.uuid4());message=str(uuid.uuid4())
    try:
        start_victim("after_commit_before_ack",name)
        compose("--profile","tools","run","--rm","--no-deps","producer",
                "--wallet",w["id"],"--player",w["playerId"],"--external",external,"--message",message)
        assert run("docker","wait",name,timeout=60)=="86", "failpoint not reached"
        assert sql("SELECT count(*) FROM inbox WHERE message_id='"+message+"' AND completed_at IS NOT NULL")=="1"
        restore(name)
        # Broker visibility is 30 seconds. Wait for an actual replay log, not only one ledger row.
        wait_condition(lambda: any(message in line and '"replay":true' in line
            for line in compose("logs","--no-color",*APPS).splitlines()), 70)
        code,r=api("POST","/wallets/"+w["id"]+"/reconciliation","wallet-service")
        assert code==200 and r["consistent"] and r["storedBalance"]["amount"]=="75.00" and r["checkedEntries"]==2
        print("PASS: commit before ACK, new process redelivered, one debit and reconciliation 75.00.")
    finally:
        restore(name)
def publisher_crash(point):
    ready()
    wait_condition(lambda: sql("SELECT count(*) FROM outbox WHERE published_at IS NULL")=="0")
    compose("stop",*APPS)
    name="wager-publisher-fault-"+uuid.uuid4().hex[:8]
    try:
        start_victim(point,name,True)
        ready("http://localhost:18080")
        w=wallet("0.00","http://localhost:18080")
        data=operation(w,"WIN","1.00")
        try:
            code,result=api("POST","/wagering/transactions","provider-a",data,data["externalTransactionId"],base="http://localhost:18080")
            assert code==200, result
        except (OSError, TimeoutError):
            # A lost HTTP response after the database commit is one of the intended outcomes.
            pass
        assert run("docker","wait",name,timeout=60)=="86"
        pending=sql("SELECT event_id FROM outbox WHERE aggregate_id='"+w["id"]+"' AND attempts=1 AND published_at IS NULL")
        assert pending, "claimed event not found"
        restore(name)
        wait_condition(lambda: sql("SELECT count(*) FROM outbox WHERE event_id='"+pending+"' AND published_at IS NOT NULL AND attempts>=2")=="1",70)
        assert sql("SELECT payload->>'eventId' FROM outbox WHERE event_id='"+pending+"'")==pending
        code,r=api("POST","/wallets/"+w["id"]+"/reconciliation","wallet-service")
        assert code==200 and r["consistent"] and r["storedBalance"]["amount"]=="1.00"
        print("PASS:",point,"lease recovered, stable eventId",pending)
    finally:
        restore(name)
p=argparse.ArgumentParser(description=__doc__)
p.add_argument("scenario", choices=["consumer","before-publish","after-publish","all"],default="all",nargs="?")
if __name__=="__main__":
    scenario=p.parse_args().scenario
    if scenario in ("consumer","all"): consumer_crash()
    if scenario in ("before-publish","all"): publisher_crash("after_outbox_claim")
    if scenario in ("after-publish","all"): publisher_crash("after_publish_before_mark")
