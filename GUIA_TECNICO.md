# Guia técnico

O serviço processa operações de apostas recebidas por HTTP ou SQS. Cada operação é registrada no PostgreSQL junto dos lançamentos da carteira e dos eventos que serão publicados. As três instâncias da aplicação compartilham o banco e as filas.

## Organização do código

| Caminho | Responsabilidade |
|---|---|
| `internal/domain/money.go` | Valores monetários em centavos, moedas e validação de limites |
| `internal/domain/model.go` | Carteiras, transações, lançamentos e regras de estado |
| `internal/application/service.go` | Abertura de carteira e processamento das operações |
| `internal/postgres` | Repositórios, transações SQL e consultas |
| `migrations` | Estrutura do banco, permissões, constraints e triggers |
| `internal/httpapi` | Rotas HTTP e tratamento das respostas |
| `internal/messaging` | Integração com SQS e validação das mensagens |
| `internal/workers` | Consumo de mensagens, publicação de eventos e retomada de pendências |
| `internal/auth` | Validação de tokens e permissões |
| `internal/bootstrap` | Composição dos módulos e ciclo de vida com Fx |

## Processamento de uma operação

1. A entrada é autenticada e validada.
2. O serviço consulta a chave de idempotência. No consumo por SQS, também consulta a inbox.
3. A carteira é bloqueada dentro da transação SQL.
4. As regras da operação são aplicadas ao saldo e ao estado da transação.
5. O resultado, os lançamentos e os eventos da outbox são gravados no mesmo commit. No fluxo SQS, a inbox também é concluída nessa transação.
6. Após o commit, a resposta HTTP é enviada ou a mensagem é confirmada. Os eventos da outbox são publicados por um worker.

O bloqueio é por carteira. Operações de carteiras diferentes podem avançar ao mesmo tempo.

## Registros e recuperação

| Registro | Função |
|---|---|
| Ledger | Histórico imutável de créditos e débitos |
| Idempotência | Guarda o resultado de uma operação para responder a novas tentativas sem aplicar outro efeito financeiro |
| Inbox | Registra o tratamento das mensagens recebidas |
| Outbox | Guarda os eventos no mesmo commit da operação para publicação posterior |
| Lease | Reserva temporariamente um evento para publicação; após expirar, permite que outro processo o assuma |
| Reconciliação | Compara o saldo armazenado com a soma dos lançamentos |

Se uma operação for confirmada no banco e a resposta se perder, a repetição com a mesma chave retorna o resultado persistido. O saldo devolvido é o da operação original, mesmo que a carteira tenha recebido outras movimentações depois.

## Exemplo de concorrência

Com saldo de 100.00, duas apostas simultâneas de 80.00 disputam a mesma carteira. A primeira confirma o débito e deixa o saldo em 20.00. A segunda lê esse saldo após a liberação do bloqueio e é rejeitada por falta de fundos. O ledger fica com o crédito de abertura e um único débito de aposta.

A repetição da aposta aprovada devolve o resultado anterior, sem gerar outro lançamento. A entrega de mensagens pode se repetir; o efeito financeiro deve permanecer único.

## Documentação

- [README.md](README.md): instalação, configuração e exemplos de chamadas.
- [ARCHITECTURE.md](ARCHITECTURE.md): regras financeiras, persistência, autenticação e tratamento de falhas.
- [TESTES.md](TESTES.md): cobertura, comandos e resultados das verificações.
