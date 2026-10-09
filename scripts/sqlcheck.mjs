import { PGlite } from '../devtools/node_modules/@electric-sql/pglite/dist/index.js';
import fs from 'node:fs';
const db = new PGlite({parsers:{20:v=>BigInt(v)}});
await db.exec("CREATE ROLE wager_app LOGIN;");
const up=fs.readFileSync('migrations/001_init.up.sql','utf8');
await db.exec(up);
console.log("PASS: PostgreSQL schema, functions, triggers and grants applied in PGlite.");
const id="00000000-0000-4000-8000-000000000001";
const p="00000000-0000-4000-8000-000000000002";
await db.exec("SET ROLE wager_app;");
await db.exec("BEGIN;");
await db.query("INSERT INTO wallets(id,player_id,currency) VALUES($1,$2,'BRL')",[id,p]);
await db.query("INSERT INTO wager_transactions(id,wallet_id,player_id,kind,amount,currency,status,result_balance,result_currency) VALUES($1,$1,$2,'OPENING',10000,'BRL','PROCESSED',10000,'BRL')",[id,p]);
await db.query("INSERT INTO wallet_ledger(id,wallet_id,transaction_id,direction,amount,currency,balance_before,balance_after) VALUES($1,$1,$1,'CREDIT',10000,'BRL',0,10000)",[id]);
await db.exec("COMMIT;");
let r=await db.query("SELECT balance,version FROM wallets WHERE id=$1",[id]);
if(r.rows[0].balance!==10000n||r.rows[0].version!==1n)throw Error(JSON.stringify(r.rows));
console.log("PASS: opening updates balance atomically with version 1.");
for(const q of ["UPDATE wallets SET balance=1","UPDATE wallet_ledger SET amount=1","DELETE FROM wallet_ledger","TRUNCATE wallet_ledger","UPDATE wager_transactions SET status='PENDING'"]){
 let failed=false;try{await db.exec(q)}catch{failed=true}if(!failed)throw Error("unguarded: "+q)
}
console.log("PASS: direct balance changes, ledger mutation and terminal transitions denied.");
const b="00000000-0000-4000-8000-000000000003",l="00000000-0000-4000-8000-000000000004";
const req=[b,id,p,"0".repeat(64)];
const insert="INSERT INTO wager_transactions(id,wallet_id,player_id,kind,amount,currency,provider_id,external_id,idempotency_key,payload_hash,round_id,game_id,status) VALUES($1,$2,$3,'BET',8000,'BRL','provider-a','tx-80','key-80',$4,'round','game','PENDING')";
await db.exec("BEGIN;");
await db.query(insert,req);
await db.query("UPDATE wager_transactions SET status='PROCESSED',result_balance=2000,result_currency='BRL' WHERE id=$1",[b]);
await db.query("INSERT INTO wallet_ledger(id,wallet_id,transaction_id,direction,amount,currency,balance_before,balance_after) VALUES($1,$2,$3,'DEBIT',8000,'BRL',10000,2000)",[l,id,b]);
await db.exec("COMMIT;");
r=await db.query("SELECT balance,version FROM wallets WHERE id=$1",[id]);
if(r.rows[0].balance!==2000n||r.rows[0].version!==2n)throw Error(JSON.stringify(r.rows));
console.log("PASS: BET ledger drives balance and version.");
await db.exec("BEGIN;");
const b2="00000000-0000-4000-8000-000000000005";
await db.query(insert.replaceAll("tx-80","tx-other").replaceAll("key-80","key-other"),[b2,id,p,"1".repeat(64)]);
await db.query("UPDATE wager_transactions SET status='PROCESSED',result_balance=0,result_currency='BRL' WHERE id=$1",[b2]);
let failed=false;try{await db.exec("COMMIT;")}catch{failed=true;await db.exec("ROLLBACK;")}
if(!failed)throw Error("missing ledger committed");
console.log("PASS: deferred constraint rejects PROCESSED without ledger.");
await db.exec("RESET ROLE;");
await db.exec(fs.readFileSync('migrations/001_init.down.sql','utf8'));
console.log("PASS: down migration.");
await db.close();
