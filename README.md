# Backend challenge em Go

API de carteiras e transações de apostas com PostgreSQL, autenticação OIDC via Keycloak e publicação/consumo de eventos com outbox e SQS FIFO.

## Estado da implementação

O repositório contém o domínio de dinheiro, carteiras e transações; casos de uso para abertura e leitura de carteiras, apostas, ganhos, perdas e reversões; adapters HTTP, PostgreSQL, Keycloak e SQS; migrations; publisher outbox; e consumidor SQS.

A cobertura atual inclui testes unitários de domínio e aplicação, testes do dispatcher outbox, testes HTTP de autorização e isolamento de provider, testes do consumidor e testes PostgreSQL de idempotência/atomicidade, concorrência entre três processos e exclusão de claims concorrentes da outbox com retry após backoff. Ainda faltam testes ponta a ponta com Keycloak, HTTP e PostgreSQL, testes com um broker SQS real, falhas controladas de publisher/consumer, reinicialização e vários cenários de recuperação descritos em [ARCHITECTURE.md](ARCHITECTURE.md). Os testes de integração PostgreSQL precisam de `TEST_DATABASE_URL` para rodar; sem essa variável, eles são ignorados.

O enunciado completo está em [CHALLENGE.md](CHALLENGE.md). As decisões e limitações técnicas estão em [ARCHITECTURE.md](ARCHITECTURE.md).

## Pré-requisitos

- Docker Desktop com Docker Compose
- Go 1.26, conforme `go.mod` e `Dockerfile`
- PowerShell no Windows para os comandos abaixo

Se ainda não existir `.env`, crie uma cópia dos valores locais de exemplo:

```powershell
if (-not (Test-Path .env)) { Copy-Item .env.example .env }
```

Não sobrescreva um `.env` existente: ele pode conter valores locais que você queira preservar. Os valores do `.env.example` servem apenas para desenvolvimento local.

Variáveis definidas em `.env`:

| Variável | Uso | Valor local de exemplo |
| --- | --- | --- |
| `POSTGRES_PASSWORD` | Senha do PostgreSQL no Compose | `backend-local-only` |
| `KEYCLOAK_ADMIN` | Usuário administrador local do Keycloak | `admin` |
| `KEYCLOAK_ADMIN_PASSWORD` | Senha local do administrador | `admin-local-only` |
| `AWS_REGION` | Região usada pelo cliente SQS | `us-east-1` |
| `SQS_ENABLED` | Habilita publisher e consumidor | `false` |
| `LOCALSTACK_AUTH_TOKEN` | Token exigido pela imagem LocalStack do perfil SQS | vazio no exemplo; necessário para ativar SQS |

## Iniciar a API sem SQS

Na raiz do repositório:

```powershell
docker compose up --build
```

Aguarde PostgreSQL e Keycloak ficarem saudáveis e a API iniciar. Endereços locais:

| Serviço | Endereço |
| --- | --- |
| API | `http://localhost:8080` |
| PostgreSQL | `localhost:5433` |
| Keycloak | `http://localhost:8081` |

Nesse modo, a API grava eventos na outbox PostgreSQL, mas não os publica. `/health/ready` verifica PostgreSQL. A migration `000001_init.up.sql` é aplicada automaticamente quando o volume do banco é criado vazio. A migration `000002_wallet_ledger_read_index.up.sql` deve ser aplicada manualmente quando necessária.

Para parar os serviços preservando os dados:

```powershell
docker compose down
```

Para apagar também o volume do banco local (isso remove os dados):

```powershell
docker compose down -v
```

## Autenticar e chamar a API

O realm local `backend` cria o client `wallet-service`, com audience `backend-api`, roles locais `wallet:write` e `wager:write`, e o provider `local-provider`. Gere um token de desenvolvimento:

```powershell
$tokenResponse = Invoke-RestMethod -Method Post -Uri 'http://localhost:8081/realms/backend/protocol/openid-connect/token' -ContentType 'application/x-www-form-urlencoded' -Body @{ grant_type = 'client_credentials'; client_id = 'wallet-service'; client_secret = 'wallet-local-secret' }
$token = $tokenResponse.access_token
```

Abra uma carteira. Use um UUID válido para `playerId`:

```powershell
$playerId = '0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1'
$walletBody = @{ playerId = $playerId; initialBalance = @{ amount = '1000.00'; currency = 'BRL' } } | ConvertTo-Json
$wallet = Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/wallets' -Headers @{ Authorization = "Bearer $token" } -ContentType 'application/json' -Body $walletBody
$wallet
```

Envie uma aposta usando o `id` retornado. O provider é determinado pelo token; não envie `providerId` no corpo. Reutilize a chave de idempotência para repetir a mesma operação com segurança:

```powershell
$betBody = @{ kind = 'BET'; externalTransactionId = 'local-bet-001'; playerId = $playerId; walletId = $wallet.id; roundId = 'local-round-001'; gameId = 'demo-game'; money = @{ amount = '25.00'; currency = 'BRL' } } | ConvertTo-Json
$headers = @{ Authorization = "Bearer $token"; 'Idempotency-Key' = 'local-provider:local-bet-001' }
Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/wagering/transactions' -Headers $headers -ContentType 'application/json' -Body $betBody
```

O mesmo corpo e a mesma chave devem devolver o resultado persistido como replay idempotente. Uma chave reutilizada com payload diferente retorna conflito. Saldo insuficiente gera uma rejeição de negócio. O valor monetário é string decimal com duas casas, por exemplo `25.00`.

## Endpoints

| Método e caminho | Acesso |
| --- | --- |
| `GET /health/live` | Público |
| `GET /health/ready` | Público |
| `GET /metrics` | Público |
| `POST /wallets` | `wallet:write` |
| `GET /wallets/{walletId}` | `wallet:write` |
| `GET /wallets/{walletId}/ledger` | `wallet:write` |
| `POST /wallets/{walletId}/reconciliation` | `wallet:write` |
| `POST /wagering/transactions` | `wager:write` |
| `GET /wagering/transactions/{transactionId}` | `wallet:write` |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | `wager:write`; provider da rota precisa corresponder ao token |

O ledger aceita `limit` de 1 a 100 (padrão 50) e devolve `nextCursor` opaco. A reconciliação compara o saldo com os lançamentos do ledger e não altera a carteira.

## Migrations

A migration inicial é automática apenas para volume vazio. Para atualizar um banco existente, aplique migrations pendentes na ordem. Execute **um comando por vez**, conforme a operação desejada.

Aplicar o índice de leitura do ledger:

```powershell
Get-Content migrations/000002_wallet_ledger_read_index.up.sql | docker compose exec -T postgres psql -U backend -d backend
```

Reverter somente esse índice:

```powershell
Get-Content migrations/000002_wallet_ledger_read_index.down.sql | docker compose exec -T postgres psql -U backend -d backend
```

A reversão da migration inicial remove tabelas e dados. Use-a somente quando realmente quiser apagar o schema:

```powershell
Get-Content migrations/000001_init.down.sql | docker compose exec -T postgres psql -U backend -d backend
```

## Testes e verificações

Testes unitários e de aplicação:

```powershell
go test ./...
```

Os testes em `internal/adapters/postgres` usam PostgreSQL real. Para executá-los, inicie o serviço e defina a URL no terminal atual:

```powershell
docker compose up -d postgres
$env:TEST_DATABASE_URL = 'postgres://backend:backend-local-only@localhost:5433/backend?sslmode=disable'
go test ./internal/adapters/postgres -count=1
```

Esses testes criam schemas com nomes aleatórios e os removem ao terminar. Aponte `TEST_DATABASE_URL` somente para um banco de teste descartável no qual o usuário possa criar e remover schemas. Para rodar toda a suíte incluindo esses testes:

```powershell
go test ./... -count=1
```

Outras verificações:

```powershell
go test -race ./...
go vet ./...
go build ./...
```

No Windows, o Controle de Aplicativo pode bloquear executáveis temporários gerados pelo `go test`. Se isso ocorrer, o teste correspondente não foi executado; não significa que passou ou falhou. Não desative a política de segurança. As verificações `go vet` e `go build` não imprimem nada quando terminam com sucesso.

`go test -race` exige CGO habilitado e um compilador C configurado. Nesta instalação, `CGO_ENABLED=0`, então o detector de corrida não pode ser executado até que o ambiente de desenvolvimento disponibilize esses pré-requisitos.

## SQS local

A integração SQS usa o perfil `sqs` do Compose e LocalStack. A imagem configurada atualmente exige um `LOCALSTACK_AUTH_TOKEN` válido. Configure-o no `.env`, defina `SQS_ENABLED=true` e inicie:

```powershell
docker compose --profile sqs up --build
```

O Compose provisiona `domain-events.fifo`, `wager-transactions.fifo` e `wager-transactions-dlq.fifo`. Sem token válido, o LocalStack pode encerrar com erro de ativação; nesse caso, o modo sem SQS continua disponível, mas não valida a integração com o broker. Não compartilhe nem commite `.env` ou tokens.
