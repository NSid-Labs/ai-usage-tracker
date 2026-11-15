# Deploy no Coolify

Segue o `docs/DEPLOY.md` do `projeto-padrao`. Este projecto é mais simples: **um
contentor, um volume, sem base de dados externa** (SQLite em `/data`).

## Duas apps

| App | Branch | Auto-deploy | Domínio |
|---|---|---|---|
| `ai-usage-staging` | `main` | ✅ sim | `staging.uso.<dominio>` |
| `ai-usage-prod` | tag `v*` | ❌ não | `uso.<dominio>` |

## Configuração da app

- **Source:** GitHub App, repositório `NSid-Labs/ai-usage-tracker`. Se a app do
  GitHub no Coolify só tiver "selected repositories", é preciso dar acesso a este
  repo antes de ele aparecer na lista.
- **Build pack:** Dockerfile (usa o `Dockerfile` do repo, multi-arch, distroless).
- **Ports exposes:** `8080`.
- **Healthcheck:** HTTP, path `/healthz`, porta `8080`, intervalo 30s, 3 tentativas.
  Não usar o `HEALTHCHECK` da imagem como única verificação: o Coolify precisa do
  path para saber quando começar a mandar tráfego.
- **Persistent storage:** volume → `/data`.
- **Restart policy:** `unless-stopped`.

## Variáveis de ambiente

Por app, no Coolify. Nunca no repositório (`.env.example` é só o contrato).

```
TRACKER_DATA_DIR=/data
TRACKER_ADDR=:8080
TRACKER_PASSWORD=<gerado, 24+ caracteres, diferente por ambiente>
TRACKER_SECRET_KEY=<openssl rand -base64 32, diferente por ambiente>
TRACKER_PUBLIC_URL=https://uso.<dominio>
TRACKER_TRUST_PROXY=true
TRACKER_POLL_INTERVAL=2m
```

`TRACKER_SECRET_KEY` **tem de existir**: cifra as credenciais guardadas (OAuth do
ChatGPT, chaves da Mistral e do OpenCode). Sem ela, o binário gera uma em
`/data/secret.key` e o backup do `.db` deixa de ser suficiente.

## Primeiro arranque

Com `TRACKER_PASSWORD` definida, o utilizador `admin` já existe: entra em
`/login` com essa password. Sem ela, o log imprime um *setup code* e o primeiro
visitante cria a conta — aceitável em staging, nunca em produção.

Depois, em **Accounts → Add account**:

1. **ChatGPT subscription (Codex)** → *Connect with sign-in*, aprovar, e colar o
   URL `localhost:1455/...` que falha ao abrir.
2. **OpenCode Go / Console** → *Connect*, abrir o link, aprovar o código e
   escolher o workspace.
3. **Mistral Vibe Code** → *API key*, com uma Vibe API key que tenha a permissão
   `see_org_inference_usage` (sem ela, `/v1/billing/budget` responde 403 e a app
   diz isso em vez de mostrar um cartão vazio).

## Deploy por API (opcional)

Token em **Keys & Tokens → API tokens** (permissão `write`).

```bash
COOLIFY=https://coolify.<dominio>
TOKEN=...

# criar app a partir do repo público
curl -s -X POST "$COOLIFY/api/v1/applications/public" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"project_uuid":"<projeto>","server_uuid":"<servidor>","environment_name":"production",
       "git_repository":"https://github.com/NSid-Labs/ai-usage-tracker",
       "git_branch":"main","build_pack":"dockerfile","ports_exposes":"8080",
       "name":"ai-usage-prod"}'
```

O resto (domínio, volume, env vars, healthcheck) faz-se na UI; são poucos campos
e um erro aqui só se descobre em produção.

## Backups

Volume `/data` → `tracker.db` (+ `-wal`/`-shm`) e, se existir, `secret.key`.
Backup diário para S3 (R2/B2), retenção 14 dias, no Coolify. Com
`TRACKER_SECRET_KEY` no Coolify, restaurar é só repor o `.db`.

## Rollback

*Deployments → Redeploy* do commit anterior. Não há migrações destrutivas: o
schema é criado com `CREATE TABLE IF NOT EXISTS` no arranque.

## Notas

- **Latência**: o polling é servidor→provider, configurável de 1 a 10 minutos.
  Abaixo disso não há dados novos para ler.
- **Segurança**: a app guarda tokens OAuth. Não expor sem TLS nem sem
  `TRACKER_PASSWORD`. A conta de leitura (viewer) serve para partilhar o
  dashboard sem dar acesso às credenciais.
