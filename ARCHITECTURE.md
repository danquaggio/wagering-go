# Arquitetura e decisões

## Fronteiras

O domínio não importa Fx, HTTP, SQS ou pgx. Campos privados protegem entidades; construtores e transições validam invariantes. RestoreWallet e RestoreTransaction reidratam sem reaplicar dinheiro nem emitir eventos.

HTTP e SQS chamam o mesmo application.Service. O caso de uso depende de interfaces Repository/Unit; o adaptador PostgreSQL usa pgx/v5 e SQL explícito. Fx compõe configuração, pool, repositório, autenticação, caso de uso, handlers e workers por módulos, construtores e hooks.

## Dinheiro

Money usa int64 em centavos e moeda. Suporta BRL, USD e EUR, todas com escala 2. Limite positivo: 92233720368547758.07; limite negativo interno: -92233720368547758.08.

Entrada exige formato decimal canônico, como "25.00": duas casas, sem sinal, espaços ou zeros à esquerda. "25", "25.0", "025.00", notação científica, NaN e Infinity são rejeitados. Não há normalização monetária nem arredondamento. Cálculos internos aceitam negativos; saldo e entrada externa não. Parsing, soma, subtração e negação verificam overflow; operações verificam moeda. JSON sempre usa strings. Persistência: BIGINT. Reconciliação: soma em NUMERIC exato antes de converter o resultado.

UUIDs são v4 e em minúsculas. IDs de negócio e chaves têm 1–128 caracteres entre letras ASCII, números, ponto, dois-pontos, hífen e sublinhado.

## Commit financeiro e concorrência

Sequência: validar identidade/entrada; iniciar SQL; conferir inbox quando houver; consultar idempotência; bloquear carteira; inserir operação; aplicar domínio; gravar estado, ledger e outbox; concluir inbox; commit. Somente depois ocorre ACK ou publicação externa.

READ COMMITTED com SELECT FOR UPDATE na linha da carteira. Carteiras distintas não compartilham lock financeiro. O repositório não atualiza saldo diretamente: INSERT do ledger dispara uma função SECURITY DEFINER que valida operação, direção, moeda, valor e saldo anterior, e atualiza saldo/versão. O agregado já calculou esse mesmo resultado. Todos os repositórios financeiros compartilham a mesma conexão e pgx.Tx.

O papel wager_app não pode editar saldo nem excluir/alterar ledger. Possui UPDATE(updated_at) porque SELECT FOR UPDATE exige permissão de atualização em alguma coluna; o trigger bloqueia até essa mudança direta. A função de ledger tem search_path fixo. A aplicação não pode criar objetos, desabilitar triggers ou truncar tabelas.

Constraints e triggers impõem:

- carteira única por jogador/moeda, saldo não negativo e versão positiva;
- dados de entrada e transações terminais imutáveis;
- equação de saldo do lançamento, valor positivo e unicidade por carteira/transação;
- exatamente um ledger para operação financeira PROCESSED, verificado no commit por constraint trigger diferido;
- nenhum ledger para LOSS, pendências ou operações sem sucesso;
- abertura única e uma reversão bem-sucedida por referência.

DBA/superusuário é uma fronteira de confiança administrativa; a aplicação nunca roda com esse papel.

Duas apostas de 80.00 sobre 100.00: o segundo escritor só lê depois que o primeiro libera a carteira. Uma processa, outra rejeita, saldo 20.00. O ledger contém um crédito OPENING e **um único débito BET**.

## Abertura

Wallet é inserida com zero/versão 1. Saldo inicial positivo cria OPENING processada, com o UUID da carteira como identidade interna estável; crédito e dois eventos são confirmados juntos. A versão permanece 1. Metadados externos são NULL e proibidos pelo schema nessa origem.

Saldo inicial zero não cria OPENING, ledger ou eventos financeiros. Outra carteira do mesmo jogador/moeda retorna 409. OPENING é proibida nas entradas externas.

## Idempotência

Unicidades: (provider_id,idempotency_key) e (provider_id,external_id). A chave enviada é mantida no registro da operação. Conteúdo e chave equivalentes reproduzem o resultado persistido, incluindo o saldo original. A consulta é autorizada antes de um replay.

Outra chave para o mesmo ID externo retorna 409, inclusive com payload igual. Essa interpretação evita aliases de chave não persistidos e impede reaplicação.

Hash financeiro: SHA-256 hexadecimal de JSON determinístico, usando mapas ordenados por encoding/json. Campos: providerId, externalTransactionId, playerId, walletId, roundId, gameId, kind, money.amount, money.currency e referenceExternalTransactionId. Referência ausente é representada por string vazia. Chave e metadados de transporte são excluídos. HTTP e SQS usam o mesmo Request.Hash; não há valores float.

Hash da inbox: JSON ordenado com messageId, type, occurredAt normalizado em UTC/RFC3339Nano, idempotencyKey e hash financeiro. Mesma identidade de envelope com outro conteúdo gera conflito sem alterar o tratamento original.

## Estados e recuperação

| Origem | Destinos |
|---|---|
| PENDING | PENDING_REFERENCE, PROCESSED, REJECTED, FAILED |
| PENDING_REFERENCE | PENDING_REFERENCE, PROCESSED, REJECTED, FAILED |
| Estado terminal | Nenhum |

Aceite sem dependência e processamento têm um único commit; não há PENDING confirmado sem continuidade. O worker também varre PENDING por defesa, além de PENDING_REFERENCE.

Tentativas e next_attempt são persistidos. Workers concorrentes bloqueiam carteira e operação nessa ordem, rechecando estado e horário. O perdedor encontra terminal ou próxima tentativa futura.

Backoff: 1, 2, 4, 8… segundos, limitado a 256. Padrão: até 12 esperas persistidas, também limitadas por TTL de 24h. A retomada após o primeiro limite atingido rejeita REFERENCE_NOT_FOUND; o prazo efetivo pode ser muito menor que 24h. A avaliação ocorre nos ciclos do worker, podendo ultrapassar o instante exato do TTL.

Referência ausente/pendente: aguardar. Referência terminal sem sucesso: REFERENCE_UNSUCCESSFUL. Referência incompatível: rejeitar. WIN com referência opcional também aguarda e exige uma BET.

Conexão, timeout, deadlock, serialização e confirmação ambígua são transitórios. HTTP retorna 503, SQS reentrega e outbox repete. Falhas desconhecidas não viram rejeições financeiras.

Para pendência já confirmada, um erro permanente de dados do PostgreSQL explicitamente identificado como 22003 ou 22023, após rollback, pode ser registrado em nova transação como FAILED/PERMANENT_STORAGE_ERROR. Essa rotina reconsulta e não altera um terminal. Mensagem inválida que nunca foi aceita segue para DLQ; não se fabrica transação para JSON ilegível.

## Referências e reversões

Resolução por (providerId,referenceExternalTransactionId). Concordam provedor, jogador, carteira, moeda e rodada. O jogo da referência pode diferir: segue-se a lista de concordâncias do enunciado. Reversões são integrais.

REFUND reverte BET. ROLLBACK inverte BET, WIN ou REFUND. A primeira reversão bem-sucedida consome a referência para ambos os tipos: REFUND e ROLLBACK da mesma BET não podem devolver o débito duas vezes.

ROLLBACK do próprio REFUND é permitido e debita o crédito do reembolso. Isso não reabre a BET para outra devolução; é uma política conservadora contra ciclos ambíguos. ROLLBACK de ROLLBACK é proibido. Reversão sem saldo usa REVERSAL_INSUFFICIENT_FUNDS; aposta sem saldo usa INSUFFICIENT_FUNDS.

LOSS exige zero e mesma moeda, emite WagerTransactionProcessed e não muda saldo, versão ou ledger.

## SQS, identidade e inbox

Filas FIFO: entrada, DLQ e saída. GroupId=walletId. O helper usa dedup IDs diferentes para comprovar recebimentos repetidos; a aplicação não depende dos cinco minutos de deduplicação do broker.

O consumidor não recebe a identidade IAM do chamador SendMessage. Na fila compartilhada, providerId sozinho seria insuficiente. Além de IAM, a mensagem exige um atributo providerSignature: HMAC-SHA256 dos bytes exatos do envelope, com chave cadastrada por provedor. A assinatura é conferida antes do domínio; um produtor não pode agir pelo outro sem sua chave. Em produção, essas chaves exigem cofre e rotação. Os valores configurados no projeto são fixtures locais.

Provisionamento IAM: app recebe/ACK na entrada e envia na saída; provedores apenas enviam na entrada; event-reader consome saída/DLQ; denied testa recusa. MiniStack usa AUTH=true.

**Limite conhecido:** MiniStack avalia políticas pelo access key, mas documenta que não verifica a assinatura SigV4. Portanto não comprova autenticação AWS real. HMAC preserva identidade dos produtores na aplicação; IAM/SigV4 reais precisam de verificação em AWS para uso em produção.

Inbox é única por consumidor/messageId. Registro, conclusão e efeitos compartilham o commit. Pendência pode concluir a entrada porque o worker assume continuidade durável. Mensagens inválidas/conflitantes não são removidas e chegam à DLQ após cinco recebimentos. Rejeição financeira confirmada é terminal e permite ACK.

Tratamento: até 10 segundos. Visibility: 30 segundos. Retry: ChangeMessageVisibility com backoff. Em cancelamento, trabalho recebido e não iniciado tem visibilidade liberada.

## Outbox

Caso de uso só grava eventos. Publisher reserva registros confirmados com FOR UPDATE SKIP LOCKED, lease de 30 segundos e token. A reserva é confirmada antes de rede; o token impede que um publisher antigo confirme a reserva de outro.

Falha antes do envio: lease expira. Falha depois do envio/antes da marca: republicação com mesmo eventId. Há retry sem descarte por limite de tentativas. Campos do snapshot são imutáveis no banco.

Destino wager-events.fifo: GroupId=walletId e DeduplicationId=eventId. Consumidores finais precisam de deduplicação persistente. Publishers concorrentes podem enviar eventos fora da ordem de ocorrência; FIFO ordena a aceitação pelo broker. Projeções devem usar walletVersion para tratar versões fora de ordem.

Tipos concretos: ProcessedData, RejectedData, PendingData e BalanceData. Construtores fixam eventType e version=1. Envelope tem os campos exigidos; aggregateId é a carteira; datas UTC; valores em strings. WalletBalanceChanged contém todos os campos pedidos. OPENING omite metadados externos.

Na retomada, correlationId é transactionId. Na entrada SQS, correlationId/causationId usam messageId. HTTP aceita X-Correlation-ID válido ou gera UUID. Identificadores financeiros permitem rastrear a cadeia mesmo sem reter o correlationId original na pendência.

## OIDC e autorização HTTP

Keycloak emite tokens client_credentials. Validamos RS256, JWKS, issuer, audience, expiração e claims de identidade. Sem emissão própria, cadastro de senhas ou confiança no providerId do corpo.

Provedor exige azp igual ao provider_id cadastrado e service_role=wager:write. Carteiras, ledger, reconciliação e métricas exigem wallet-service/wallet:manage. O serviço interno não pode usar rotas de provedor como atalho.

Consultas por UUID filtram provider_id e devolvem 404 para outro provedor. A rota com provider explícito devolve 403 se não corresponder à identidade. Replays passam pela mesma autorização. PLAYER_MISMATCH não devolve saldo da carteira incorreta.

## HTTP e códigos de falha

| Situação | HTTP | Corpo |
|---|---:|---|
| Abertura | 201 | Carteira |
| Processada/replay | 200 | transactionId, status, balance, idempotentReplay |
| Pendência | 202 | transactionId, status, idempotentReplay |
| Rejeição de negócio | 422 | Resultado persistido + failureCode |
| FAILED no envio/replay | 500 | Resultado persistido + failureCode |
| Entrada corrigível | 400 | failureCode |
| Token ausente/inválido/expirado | 401 | UNAUTHENTICATED |
| Sem permissão | 403 | FORBIDDEN |
| Recurso ausente/inacessível | 404 | NOT_FOUND |
| Chave/ID externo em conflito | 409 | IDEMPOTENCY_CONFLICT |
| Carteira duplicada | 409 | WALLET_ALREADY_EXISTS |
| Falha transitória | 503 | TEMPORARILY_UNAVAILABLE; Retry-After: 1 |

GET de transação existente retorna 200 com o estado, mesmo REJECTED/FAILED. JSON malformado, campos desconhecidos e Money inválido são INVALID_INPUT antes do aceite. Carteira inexistente não cria transação; em SQS, é entrada inválida encaminhada à DLQ.

Corrigíveis: INVALID_INPUT, INVALID_IDEMPOTENCY_KEY, INVALID_IDENTIFIERS, INVALID_KIND, INVALID_AMOUNT, INVALID_CURRENCY, INVALID_LOSS, REFERENCE_REQUIRED, INVALID_REFERENCE, INVALID_WALLET, INVALID_WALLET_ID, INVALID_TRANSACTION_ID, INVALID_CURSOR e INVALID_LIMIT.

Definitivos após aceite: INSUFFICIENT_FUNDS, REVERSAL_INSUFFICIENT_FUNDS, PLAYER_MISMATCH, CURRENCY_MISMATCH, REFERENCE_MISMATCH, REFERENCE_UNSUCCESSFUL, INVALID_REFERENCE_KIND, REVERSAL_AMOUNT_MISMATCH, ALREADY_REVERSED, REFERENCE_NOT_FOUND, MONEY_OVERFLOW e VERSION_OVERFLOW. FAILED usa PERMANENT_STORAGE_ERROR. Replays não reavaliam esses resultados.

Ledger: limite padrão 50, máximo 200; cursor Base64URL opaco com carteira e seq, validado contra a rota; ordem ascendente estável. Reconciliação usa uma única consulta/snapshot MVCC, inclui abertura e calcula stored-calculated. Não corrige saldo; divergências geram resposta, log e métrica.

## Operação e limitações

Logs JSON têm identificadores/status, sem tokens, senhas, corpo financeiro ou detalhes de erros de banco que exponham linhas. Métricas: status, duplicatas, retries, tamanho aproximado da DLQ, conflitos, idade da outbox, soma/contagem de latência e divergências. São contadores operacionais por processo; ledger/transações são a auditoria durável.

Startup valida configuração, banco migrado, filas e JWKS. Live verifica processo; ready verifica PostgreSQL e SQS. SIGTERM interrompe novas buscas/aceitação HTTP, aguarda ou cancela trabalho e fecha o pool ao final. Fx tem 30 segundos; Compose concede 40.

Não foram implementados tracing, dashboards, partidas dobradas ou benchmark. A latência tem soma/contagem, sem quantis. O runner de migrations tem uma versão. Recursos de produção como HTTPS, cofre/rotação, backups e políticas reais da AWS ficam fora deste laboratório.

A suíte de integração em containers e os três cenários de interrupção passaram na execução de 08/10/2026. A cobertura, os comandos e os resultados estão em [TESTES.md](TESTES.md).

## Fontes primárias

- [Fx lifecycle](https://uber-go.github.io/fx/lifecycle.html) e [módulos](https://uber-go.github.io/fx/modules.html).
- [PostgreSQL — locks](https://www.postgresql.org/docs/current/explicit-locking.html).
- [Keycloak — OIDC](https://www.keycloak.org/securing-apps/oidc-layers).
- [AWS — deduplicação SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/using-messagededuplicationid-property.html).
- [MiniStack — configuração e IAM](https://ministack.org/docs/configuration).
