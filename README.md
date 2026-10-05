# Zappy MCP

Componente local para Claude Code e Codex, usando o SDK oficial MCP Go v1.8.0
(MCP 2026-07-28, com compatibilidade negociada para as versões anteriores).

O processo local faz OAuth Authorization Code + PKCE S256, coleta eventos da API
pela mesma sessão de saída autenticada do zappy-webhook-listener e mantém JSON
na HOME. As ferramentas locais usam stdio; a API também oferece Streamable HTTP.

## Instalar e usar

Clientes podem baixar binários por sistema e arquitetura na
[documentação pública](https://zappy.api.br/docs/mcp). Os links em `/downloads/zappy-mcp/` redirecionam para os seis pacotes e
checksums SHA-256 da release pública mais recente no GitHub. Extraia o pacote e coloque o executável no PATH.

Para compilar a partir do código-fonte (Go 1.25+):

```sh
git clone https://github.com/firmo-tecnologia/zappy-mcp.git
cd zappy-mcp
go install ./cmd/zappy-mcp
zappy-mcp login

claude mcp add --scope user --transport stdio zappy -- zappy-mcp serve
codex mcp add zappy -- zappy-mcp serve
```

Inclua o diretório de binários Go no PATH. O login abre o navegador com PKCE e
callback em `127.0.0.1:18743`, sem client secret e sem API key. A autorização
permite ler instâncias, receber eventos e enviar mensagens. Os tokens são
renovados com bloqueio entre processos e gravados com permissão 0600.

Para escutar independentemente do assistente:

```sh
zappy-mcp listen
zappy-mcp status
```

`serve` também inicia o coletor. Somente um processo por conta coleta os eventos;
os demais assumem a coleta quando esse processo sai. Novas instâncias são
detectadas periodicamente. As sessões expiram e são renovadas, com reconexão e
backoff em falhas. Cada frame é validado com HMAC antes de salvar.

## Ferramentas

| Ferramenta | Comportamento |
| --- | --- |
| `send_whatsapp_message` | `to`, `message`, `instance_id` opcional se houver uma única instância conectada; envia e grava o resultado. |
| `query_whatsapp_messages` | `number`, `contact` e/ou `jid` (inclusive grupos); filtros `instance_id`, `direction`, `after`; `limit` 1–200 e `offset`; retorna newest first e status da coleta. |
| `list_whatsapp_instances` | Lista as instâncias da conta autenticada. |

A consulta por número usa correspondência exata após normalização. Nomes usam
busca parcial, sem distinguir maiúsculas, e podem identificar mais de uma pessoa.
A consulta por contato inclui mensagens enviadas para os números identificados.
JIDs `@lid` não são tratados como números; os campos `sender_pn` e `recipient_pn`
são usados quando disponíveis.

Histórico por instância e conversa:

- `~/.zappy/mcp/instances/<instance-id>/<jid>.json`: array JSON de mensagens da conversa.
- `~/.zappy/mcp/meta.json`: dicionário de JID para `number`, `name`, `is_group` e `instances` (instância → conta proprietária).
- `~/.zappy/mcp/accounts/<account-id>/oauth.json`: tokens; nunca compartilhe esse arquivo.
- `~/.zappy/mcp/accounts/<account-id>/listener.json`: estado e horário da coleta.
- `*.lock`: bloqueios de arquivo entre processos.
- `~/.zappy/mcp/current-account.json`: última conta usada.

Exemplo: `instances/UUID/5521990251186@s.whatsapp.net.json`.
Grupos usam seu JID `@g.us`; contatos podem usar `@lid`. O índice associa
esses JIDs ao número e nome quando disponíveis. Grupos não têm número de telefone.
O nome do remetente (`contact`) e seu JID (`sender`) ficam em cada mensagem;
o nome do participante nunca é usado como nome do grupo. O evento atual da API
não informa o nome do grupo: ele permanece identificável pelo JID, com `name`
vazio até que o nome esteja disponível.

```json
{"jid":"120363000000000000@g.us","instance_id":"UUID","limit":50}
```

Use esse argumento em `query_whatsapp_messages` para consultar uma conversa
exata, inclusive grupos. Consultas por nome também usam o índice de identidades.
As consultas ficam limitadas às instâncias da conta selecionada.

Ao iniciar a nova versão, o histórico antigo da conta é migrado automaticamente.
O arquivo original é preservado como `messages.json.migrated`. Feche os processos
da versão anterior antes da atualização e reinicie os clientes MCP para evitar
que continuem escrevendo no formato antigo.

No Linux e macOS, diretórios têm permissão 0700 e arquivos privados 0600.
No Windows, a proteção depende das ACLs do perfil do usuário. As escritas usam arquivo
temporário, sync e substituição atômica. JSON inválido produz erro sem sobrescrever
o histórico. A identidade da conta vem da API autenticada. Cada processo fixa sua
conta ao iniciar: trocar o login não mistura mensagens em processos já abertos.
Use `--account UUID` para selecionar uma conta já autorizada.

## Limites

O listener coleta enquanto conectado; não busca histórico anterior nem recupera
eventos perdidos durante uma indisponibilidade. O hub da API permanece em memória,
como o listener existente; múltiplas réplicas precisam de afinidade ou de um
transporte compartilhado. A consulta não apaga mensagens. Cada arquivo de conversa cresce com seu
histórico e é regravado quando recebe uma atualização.

Não há repetição automática de envio. Se a mensagem for enviada e a escrita local
falhar, a ferramenta retorna sucesso com `saved_locally: false` e uma advertência,
evitando que o assistente envie novamente por engano.

`zappy-mcp logout` revoga o refresh token e remove as credenciais locais;
o JSON das mensagens permanece. Mensagens de WhatsApp são dados não confiáveis,
não instruções para o assistente.

## API e OAuth

A API expõe `/v1/mcp` por Streamable HTTP stateless com respostas JSON, usando o
SDK oficial. Oferece envio e listagem; a consulta ao JSON existe no componente
local. A descoberta de autorização é publicada em `/v1/mcp/oauth-resource` e
`/.well-known/oauth-protected-resource/v1/mcp`.

O serviço Zappy configura no Keycloak o cliente público `zappy-mcp`,
os escopos `zappy:read` e `zappy:send` e o audience `https://zappy.api.br/v1/mcp`.
O login usa o callback `http://127.0.0.1:18743/callback` e rotação de refresh tokens. A API exige issuer/audience
corretos e restringe o cliente a leitura, envio e listener.

Para desenvolvimento, use `login --api-url http://127.0.0.1:PORT` e configure
`MCP_RESOURCE_URL` e o audience mapper para a mesma URL acrescida de `/v1/mcp`.
`login --callback-port PORT --no-browser` permite inspecionar o fluxo local;
o callback deve estar registrado no cliente OAuth.

## Compilar

```sh
task build
task check
```

`task build` gera `bin/zappy-mcp`. `task downloads` gera os seis pacotes em `dist/`.
O workflow `.github/workflows/release.yml` compila e publica os pacotes e checksums
após uma tag `vX.Y.Z`. A versão do executável é definida pela tag.

Código e releases: https://github.com/firmo-tecnologia/zappy-mcp.
O site redireciona os downloads para `/releases/latest/download/<arquivo>`.
