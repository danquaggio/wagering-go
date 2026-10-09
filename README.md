# Apostas distribuídas em Go

Serviço de processamento de apostas com entrada por HTTP e SQS, persistência em PostgreSQL e autenticação OIDC pelo Keycloak. O projeto usa Fx para compor os módulos e gerenciar o ciclo de vida das três instâncias da aplicação.

O [guia técnico](GUIA_TECNICO.md) apresenta a estrutura do código e o fluxo das operações. As regras e decisões estão em [ARCHITECTURE.md](ARCHITECTURE.md), e os comandos de validação e resultados em [TESTES.md](TESTES.md).

## Início rápido no Windows

Pré-requisitos: Docker Desktop com containers Linux e Compose v2. Python 3.10+ somente para scripts executados no computador. Go 1.26.8 somente para executar comandos Go fora do Docker.

Extraia o ZIP e abra o terminal **na pasta wagering**:

```powershell
Copy-Item .env.example .env
docker compose up --build -d
docker compose logs -f app-1 app-2 app-3
```

Linux/macOS: use `cp .env.example .env`. Espere o log `application started`. Ctrl+C encerra a visualização dos logs, sem parar a stack.

```text
docker compose --profile tools run --rm demo
```

A demonstração cria 100.00, aposta 25.00, repete a aposta e reconcilia em 75.00. Esse comando não precisa de Python ou Go instalados no host.

| Serviço | Endereço |
|---|---|
| API, processos 1 / 2 / 3 | http://localhost:8080 / http://localhost:8082 / http://localhost:8083 |
| Keycloak | http://localhost:8081 |
| PostgreSQL / DBeaver | localhost:5432, banco wagering |
| SQS / MiniStack | http://localhost:4566 |

O Compose provisiona banco, migrations, filas, DLQ, identidades e três processos independentes. As portas estão vinculadas a 127.0.0.1. É um laboratório com HTTP e credenciais de exemplo; não exponha na internet.

## Chamadas autenticadas em PowerShell

```powershell
$tokenUrl = "http://localhost:8081/realms/wagering/protocol/openid-connect/token"
$interno = (Invoke-RestMethod -Method Post -Uri $tokenUrl -Body @{
  grant_type="client_credentials"
  client_id="wallet-service"
  client_secret="wallet-service-local-secret"
}).access_token
$provedor = (Invoke-RestMethod -Method Post -Uri $tokenUrl -Body @{
  grant_type="client_credentials"
  client_id="provider-a"
  client_secret="provider-a-local-secret"
}).access_token
$carteira = Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wallets" `
  -Headers @{Authorization="Bearer $interno"} -ContentType "application/json" `
  -Body (@{
    playerId=[guid]::NewGuid().ToString()
    initialBalance=@{amount="100.00"; currency="BRL"}
  } | ConvertTo-Json)
$externo = [guid]::NewGuid().ToString()
$corpo = @{
  providerId="provider-a"; externalTransactionId=$externo
  playerId=$carteira.playerId; walletId=$carteira.id
  roundId="round-demo"; gameId="game-demo"; kind="BET"
  money=@{amount="25.00"; currency="BRL"}
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wagering/transactions" `
  -Headers @{Authorization="Bearer $provedor"; "Idempotency-Key"=$externo} `
  -ContentType "application/json" -Body $corpo
```

Repita a última chamada no processo 2, usando a porta 8082. Deve retornar `idempotentReplay: true` sem outro débito.

```powershell
Invoke-RestMethod -Method Post `
  -Uri "http://localhost:8080/wallets/$($carteira.id)/reconciliation" `
  -Headers @{Authorization="Bearer $interno"}
Invoke-RestMethod `
  -Uri "http://localhost:8080/wallets/$($carteira.id)/ledger?limit=50" `
  -Headers @{Authorization="Bearer $interno"}
```

Tokens duram 300 segundos. O provedor B usa provider-b/provider-b-local-secret. O administrador do Keycloak é admin, com a senha local de .env. short-lived e wrong-audience são clientes de teste. A aplicação não utiliza o administrador do IdP.

## Enviar por SQS

O envelope do enunciado é preservado, com atributo adicional `providerSignature`: HMAC-SHA256 hexadecimal dos bytes UTF-8 exatos do corpo, usando a chave do provedor. O helper assina automaticamente:

```powershell
docker compose --profile tools run --rm producer `
  --wallet $carteira.id --player $carteira.playerId `
  --external $externo --key $externo --amount "25.00" --repeat 3
```

Nesse exemplo, HTTP e SQS recebem a mesma operação. IDs de deduplicação diferentes no transporte forçam entregas reais; a aplicação deve evitar o débito duplicado. Para repetir uma identidade de envelope, reutilize o corpo exato: occurredAt faz parte do hash da inbox.

Filas: wager-transactions.fifo, wager-transactions-dlq.fifo e wager-events.fifo. Visibility timeout: 30 segundos. Redrive: cinco recebimentos.

## Testes

Testes unitários e composição, sem containers:

```text
go test ./...
go test -race ./...
go vet ./...
```

Com a stack ligada, execute infraestrutura em containers, autorização real e três processos:

```text
docker compose --profile test run --rm test
```

O comando executa `go test -race -count=1 -tags=integration ./... -timeout=10m`. A suíte não substitui PostgreSQL, IdP e SQS por mocks. Cria dados próprios e bancos temporários para migrations. A credencial administrativa do banco só é entregue ao executor de testes.

Simulações de interrupção, na pasta do projeto:

```text
python scripts/chaos.py all
```

No Windows, também pode ser `py scripts/chaos.py all`. O script para as três APIs, inicia um processo com failpoint, aguarda saída 86 e restaura a stack. Não execute outra suíte simultaneamente. Cenários individuais: consumer, before-publish e after-publish.

Para testes apenas de banco fora do Docker, configure TEST_DATABASE_URL e TEST_ADMIN_DATABASE_URL, apontando para PostgreSQL real já migrado:

```text
go test -race -count=1 -tags=dbtest ./tests/integration
```

Para integração completa fora do Docker, configure também TEST_API_URLS (três URLs), TEST_TOKEN_URL e as variáveis da aplicação. Prefira o serviço test: o MiniStack anuncia URLs com hostname ministack, resolvido na rede do Compose.

Checagem SQL auxiliar, sem Docker:

```text
npm ci --prefix devtools
node scripts/sqlcheck.mjs
```

Ela usa PostgreSQL compilado para WebAssembly, não comprova concorrência e não substitui a integração obrigatória.

## Migrations

Aplicação:

```text
docker compose run --rm migrate up
```

O SQL roda em uma transação, como proprietário do schema. O serviço normal usa wager_app com privilégios restritos.

A reversão seguinte **apaga os dados financeiros do laboratório**:

```text
docker compose stop app-1 app-2 app-3
docker compose run --rm -e CONFIRM_DESTRUCTIVE_DOWN=yes migrate down
docker compose run --rm migrate up
docker compose start app-1 app-2 app-3
```

Faça backup antes se quiser preservar dados. Não execute migradores concorrentes. O runner contempla a migration atual; novas versões precisam ser acrescentadas ao runner.

## Configuração e execução fora do Docker

| Variável | Uso |
|---|---|
| DATABASE_URL | Banco com papel wager_app |
| HTTP_ADDR | Listener HTTP |
| OIDC_ISSUER / OIDC_JWKS_URL | Issuer exato / chaves públicas do IdP |
| OIDC_AUDIENCE | wagering-api |
| AWS_REGION / AWS_PROFILE / AWS_SHARED_CREDENTIALS_FILE | Identidade IAM |
| SQS_ENDPOINT | Endpoint do emulador; omitir para AWS |
| SQS_QUEUE / SQS_OUTPUT_QUEUE / SQS_DLQ | Nomes das filas |
| PROVIDER_SIGNING_KEYS | JSON de chaves HMAC, mínimo 32 bytes por provedor |
| REFERENCE_MAX_ATTEMPTS | Limite de esperas persistidas; padrão 12 |
| REFERENCE_TTL | Prazo de espera; padrão 24h |
| SQS_WORKERS | Consumidores por processo; padrão 4 |
| ALLOW_FAILPOINTS / FAILPOINT | Apenas ensaios de interrupção |

.env.example também fornece exemplos para `go run ./cmd/wager`. O Go não carrega .env automaticamente: exporte as variáveis no terminal. Para obter as credenciais locais geradas, crie artifacts e execute:

```text
docker compose cp app-1:/run/aws/credentials ./artifacts/credentials
```

## Organização

| Diretório | Responsabilidade |
|---|---|
| internal/domain | Dinheiro, carteira, transação, ledger e eventos |
| internal/application | Caso de uso e unidade transacional |
| internal/postgres | SQL, pgx, consultas e outbox |
| internal/auth, httpapi, messaging | Autenticação e entradas |
| internal/workers, bootstrap | Background e ciclo de vida Fx |
| migrations, infra | Schema e provisionamento |
| tests/integration, scripts | Verificações, demonstração e falhas |

As garantias e decisões de implementação estão em [ARCHITECTURE.md](ARCHITECTURE.md).
