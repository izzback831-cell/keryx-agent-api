# Connect AI assistants to Keryx

Set up your assistant with the hosted service's HTTPS URL and API key. The operator runs the Keryx node and the MCP server. You can submit inference requests from Claude Code, Codex, VS Code with Copilot, Cursor, Cline, or Gemini CLI without installing a Keryx node.

Each account has a dedicated Keryx wallet managed by the operator. The hosting server handles the prompt and the response in plaintext. Requests to the Keryx network and responses from miners use the Keryx protocol's encryption. The service pays for requests with KRX deposited into the account's wallet. One KRX equals 100,000,000 sompi.

## Configure your MCP client

Ask the operator for your API key and the HTTPS URL ending in `/mcp`. Set `KERYX_API_KEY` in your environment when the client configuration references it. The Cline example uses a placeholder for the key in the client's local settings. Replace `https://api.example.org/mcp` with the service URL and merge the example with your existing settings.

| Client | Example configuration | Where to add it |
| --- | --- | --- |
| Claude Code | `configs/claude-code.json` | Project `.mcp.json` or `claude mcp add` |
| Codex CLI and editor extension | `configs/codex.toml` | `~/.codex/config.toml` |
| GitHub Copilot CLI | `configs/copilot.json` | `~/.copilot/mcp-config.json` or project `.mcp.json` |
| VS Code with Copilot | `configs/vscode.json` | `.vscode/mcp.json` |
| Cursor | `configs/cursor.json` | `~/.cursor/mcp.json` or project `.cursor/mcp.json` |
| Cline | `configs/cline.json` | Remote servers tab in Cline's MCP settings |
| Gemini CLI | `configs/gemini.json` | `~/.gemini/settings.json` |

## Submit an inference request

Ask your assistant “List the available Keryx models and their costs.” It calls `keryx_models`. Then ask “Show my Keryx account” to get the wallet's deposit address, network, and spending limits. Fund the wallet on the network shown in the account details.

For a first request, try “Use Keryx to get a second opinion on this function. Use tier 0, limit the response to 256 tokens, and spend at most 0.8 KRX.” The assistant calls `keryx_ask`, saves the returned request ID, and waits at least three seconds before calling `keryx_result`. The response becomes available after the network processes the request.

| Tool | Purpose |
| --- | --- |
| `keryx_models` | List models, availability, and the minimum cost in sompi |
| `keryx_account` | Get the account's deposit address, network, and spending limits |
| `keryx_ask` | Submit inference with `prompt`, `tier`, `request_key`, `max_cost_sompi`, and optionally `max_tokens` |
| `keryx_result` | Get the status and decrypted response for a request owned by the account |

If a submission times out, retry with the **same `request_key` and parameters**. The service reuses the signed transaction. A new key starts a new billable request. Reserved amounts count toward the UTC day when the request was created, including preparations still pending. The wallet reserves inputs used in signed transactions to prevent concurrent reuse.

The endpoint uses MCP Streamable HTTP with JSON responses and supports protocol versions 2025-03-26, 2025-06-18, and 2025-11-25. Poll `keryx_result` for requests that take longer. Tests cover authentication, tool calls, retries, and account isolation through simulated MCP exchanges. Live validation in each client with a synced Keryx node and active miners is still pending.

## Set up the hosted service

Build the API relay and the wallet component using the instructions in [README.md](README.md). On Windows x64, keep both executables together in `bin/windows-amd64`. On Linux x86-64, the build script places both binaries in `bin/linux-amd64`. The script requires Go 1.26 or later, Rust 1.96 or later, a C compiler, and network access to GitHub and the dependency registries. Run it from the repository root.

```bash
sh ./build-linux.sh
```

Use a synced Keryx node with `--utxoindex`. The relay uses JSON wRPC and the wallet component uses Borsh wRPC by default. Keep the node's wRPC endpoints private.

| Network | JSON wRPC port | Borsh wRPC port |
| --- | --- | --- |
| Mainnet | `24110` | `23110` |
| Testnet-10 | `24210` | `23210` |

Set `KERYX_RPC_URL` for the relay and `KERYX_AGENT_RPC_URL` and `KERYX_AGENT_RPC_ENCODING` for the wallet component if you need to override the defaults.

### Create and fund an account

The examples create a mainnet account with spending limits of 3 KRX per request and 20 KRX per UTC day. The command prints the API key and public deposit address. Use lowercase letters, digits, and hyphens in the account name. Give the account holder the API key privately.

**Windows PowerShell**

```powershell
$env:KERYX_API_DATA_DIR = Join-Path $PWD 'keryx-service-mainnet'
.\bin\windows-amd64\keryx-agent-api.exe account-create user1 300000000 2000000000
$env:KERYX_TRUST_CADDY = '1'
.\bin\windows-amd64\keryx-agent-api.exe
```

**Linux**

```bash
export KERYX_API_DATA_DIR="$PWD/keryx-service-mainnet"
./bin/linux-amd64/keryx-agent-api account-create user1 300000000 2000000000
KERYX_TRUST_CADDY=1 ./bin/linux-amd64/keryx-agent-api
```

The wallet needs enough spendable KRX to cover the request cost and at least 1 KRX in change. The change returns to the account's wallet. The account holder can look up the deposit address with `keryx_account`. Protect and back up the service data directory so wallets and requests survive restarts and restores. The `wallets` directory contains the private keys. The file `accounts/<name>.json` contains the API key hash and spending limits. Set its `disabled` field to disable the account.

### Publish the HTTPS endpoint

Configure the domain in the supplied Caddy setup and start Caddy as described in [README.md](README.md#publish-the-service-over-https). Share the public MCP URL, `https://YOUR_DOMAIN/mcp`, with users. For clients that send an `Origin` header, list the permitted origins in `KERYX_MCP_ORIGINS`, separated by commas. Desktop MCP clients typically omit that header.

## Hosted HTTP API

The same account can call the HTTP API with `Authorization: Bearer YOUR_API_KEY`.

| Route | Purpose |
| --- | --- |
| `GET /v1/account` | Get account information |
| `POST /v1/agent/requests` | Submit JSON with `request_key`, `prompt`, `tier`, `max_cost_sompi`, and optionally `max_tokens` |
| `GET /v1/agent/requests/{request_id}` | Get the result for a request owned by the account |

The server stores up to 4,096 hosted request records to preserve retry IDs. It rejects new requests once that capacity is reached. The native relay tracks accepted requests for 48 hours.

## Client references

- [Claude Code MCP](https://code.claude.com/docs/en/mcp)
- [Codex MCP](https://developers.openai.com/codex/mcp)
- [VS Code and GitHub Copilot MCP configuration](https://code.visualstudio.com/docs/agents/reference/mcp-configuration)
- [Cursor MCP](https://cursor.com/docs/mcp)
- [Cline MCP](https://docs.cline.bot/mcp/mcp-overview)
- [Gemini CLI MCP](https://geminicli.com/docs/tools/mcp-server/)
