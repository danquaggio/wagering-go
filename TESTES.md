# Testes

Última execução registrada: 08/10/2026.

## Resultados

A demonstração, a suíte de integração e os três cenários de interrupção passaram no ambiente com Docker Compose no Windows. Os trechos de saída estão em [evidence/docker-compose-2026-10-08.txt](evidence/docker-compose-2026-10-08.txt).

| Verificação | Resultado |
|---|---|
| Inicialização | PostgreSQL, MiniStack e Keycloak saudáveis; três aplicações iniciadas |
| Demonstração da API | Abertura com 100.00, aposta de 25.00 e saldo final de 75.00 |
| Repetição da aposta | Mesmo transactionId, idempotentReplay=true e nenhum novo débito |
| Reconciliação | Saldo calculado igual ao armazenado, diferença de 0.00 e duas entradas no ledger |
| Suíte de integração com `-race` | Aprovada em 78.238s, sem falhas ou corrida de dados reportadas |
| Interrupção após commit, antes do ACK | Reentrega a outro processo, um débito e reconciliação em 75.00 |
| Interrupção antes da publicação | Lease recuperado e eventId preservado |
| Interrupção após publicar, antes de registrar a publicação | Lease recuperado e eventId preservado |

## Execução

Na pasta do projeto:

```text
docker compose up --build -d
docker compose --profile tools run --rm demo
docker compose --profile test run --rm test
python scripts/chaos.py all
```

No Windows, o último comando também pode ser executado com `py scripts/chaos.py all`. Os cenários de interrupção param e reiniciam as aplicações; devem rodar sem outra suíte ativa.

O serviço `test` executa:

```text
go test -race -count=1 -tags=integration ./... -timeout=10m
```

Para os testes unitários, sem containers:

```text
go test -race -count=1 ./...
go vet ./...
```

## Cobertura

| Cenário | Teste ou arquivo |
|---|---|
| Dinheiro, limites, moedas e parsing | `internal/domain/money_test.go` |
| Carteira, estados, abertura, ledger e reversões | `internal/domain/model_test.go` |
| Hash HTTP/SQS e assinatura do provedor | `internal/messaging/broker_test.go` |
| Composição dos módulos Fx | `TestFxComposition` |
| Regras financeiras e replay do saldo original | `TestFinancialRules` |
| Inbox e rollback atômico | `TestInboxAtomicityAndConflict` |
| Referência atrasada e expiração | `TestPendingReferenceDurability` |
| Restrições do banco e rollback financeiro | `TestDatabaseProtection` |
| Progresso de uma carteira enquanto outra está bloqueada | `TestIndependentWalletProgress` |
| 50 envios da mesma aposta em três processos | `TestThreeProcesses50Duplicates` |
| Duas apostas de 80.00 sobre saldo de 100.00 | `TestThreeProcessesTwoBets80` |
| Operações em carteiras distintas | `TestThreeProcessesIndependentWallets` |
| Migrations, proteção do ledger, concorrência e lease da outbox | `TestMigrationsAndConcurrentOutbox` |
| Configuração do banco isolado, em URL e chave/valor | `TestDatabaseConfigIsolation` |
| OIDC, token expirado e isolamento entre provedores | `TestRealOIDCAndIsolation` |
| HTTP e entregas repetidas por SQS | `TestHTTPAndSQSRepeatedDeliveries` |
| IAM, erro permanente e DLQ | `TestBrokerPolicyAndDLQ` |
| Inicialização e encerramento com Fx | `TestRealFxStartStop` |
| Commit antes do ACK | `python scripts/chaos.py consumer` |
| Lease abandonado antes de publicar | `python scripts/chaos.py before-publish` |
| Publicação sem confirmação local | `python scripts/chaos.py after-publish` |

O teste de duplicatas distribui 17, 17 e 16 chamadas entre três subprocessos, cada um com seu pool de conexões. O teste SQS utiliza IDs de deduplicação diferentes e verifica o aumento dos contadores de replay, confirmando o recebimento das repetições.

Nos testes da outbox, uma nova tentativa precisa manter o mesmo eventId. A deduplicação FIFO pode suprimir uma entrega dentro da janela do SQS; isso não implica entrega exatamente uma vez fora dessa janela.

## Isolamento dos bancos de teste

Na primeira execução da integração, `TestMigrationsAndConcurrentOutbox` encontrou zero reservas do evento esperado. O processo principal usava um banco temporário, mas os subprocessos ainda recebiam a conexão do banco original.

A causa era o uso de `ConnString()` após alterar `ConnConfig.Database`: o método devolve a string de conexão original. O nome do banco temporário passou a ser transmitido por `TEST_DATABASE_NAME` e aplicado na criação de cada pool. O helper também verifica `current_database()`.

O ajuste foi aplicado ao teste de outbox e ao de referências pendentes. O teste de configuração passou com `-race`, e a execução seguinte da suíte completa terminou com `ok example.com/wagering/tests/integration 78.238s`.

## Verificações adicionais

As verificações abaixo foram executadas em Linux amd64 com Go 1.26.8, separadamente da execução em Docker no Windows.

| Verificação | Resultado |
|---|---|
| Compilação do executável com `-buildvcs=false` | Aprovada |
| `go test -race -count=1 ./...` | Aprovado |
| `go vet -tags=integration ./...` | Aprovado |
| `go test -tags=integration -run '^$' ./...` | Compilação dos testes de integração aprovada; esse comando não executa os testes |
| Teste de configuração do banco isolado com `-race` | Aprovado |
| Formatação Go e sintaxe dos scripts Python | Verificadas |
| JSON do Keycloak e YAML do Compose | Estruturas verificadas |
| Checagem auxiliar com PGlite 0.3.14 | Migrations, permissões, imutabilidade e regras de ledger aprovadas |

Os registros estão em `evidence/`. A opção `-buildvcs=false` desabilita a inclusão de metadados Git no binário e também é usada no Dockerfile. A checagem com PGlite valida SQL e triggers, mas não substitui os testes de concorrência com PostgreSQL.
