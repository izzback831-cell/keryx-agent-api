# Keryx Agent API 0.3

Keryx Agent API provides a public HTTP API and an MCP server for Keryx inference requests. Claude Code, Codex, VS Code with Copilot, Cursor, Cline, and Gemini CLI can connect over HTTPS with an API key. In hosted mode, the service prepares transactions and manages a dedicated wallet for each account. For connection instructions and client configuration examples, see [MCP.md](MCP.md).

The native `/v1/requests` endpoints accept transactions signed by the caller. In native mode, the agent keeps the wallet on its own machine. The hosting domain and Keryx node configuration apply to both modes.

| Path | Purpose |
| --- | --- |
| `bin/windows-amd64/keryx-agent-api.exe` | Windows x64 API server |
| `bin/linux-amd64/keryx-agent-api` | Linux x86-64 API server |
| `bin/windows-amd64/keryx-agent.exe` | Windows wallet component and native command-line client |
| `agent/` | Wallet component and native client source for Windows and Linux |
| `relay/` | Server source and OpenAPI specification |
| `Caddyfile` | HTTPS reverse proxy configuration |

## Build from source

Install Go 1.26 or later, Rust 1.96 or later, and a C compiler. On Windows x64, install the Visual Studio C++ Build Tools for the MSVC linker. Run these commands in PowerShell from the repository root.

```powershell
New-Item -ItemType Directory -Force -Path bin/windows-amd64 | Out-Null
Push-Location relay
go build -mod=readonly -trimpath -o ../bin/windows-amd64/keryx-agent-api.exe .
Pop-Location
cargo build --locked --release --manifest-path agent/Cargo.toml
Copy-Item agent/target/release/keryx-agent.exe bin/windows-amd64/
```

On Linux x86-64, run `sh ./build-linux.sh` from the repository root. The relay and the wallet component are built into `bin/linux-amd64/`.

## Start the API server

The server defaults to mainnet. Use a synced Keryx node that listens for JSON wRPC on `127.0.0.1:24110`. Add `--rpclisten-json=127.0.0.1:24110` to the node's startup command if you need to enable that listener. The API server listens for HTTP on `127.0.0.1:8787`. When using the supplied Caddy proxy, set `KERYX_TRUST_CADDY=1`.

**Windows PowerShell**

```powershell
$env:KERYX_TRUST_CADDY = '1'
.\bin\windows-amd64\keryx-agent-api.exe
```

**Linux**

```bash
chmod +x bin/linux-amd64/keryx-agent-api
KERYX_TRUST_CADDY=1 ./bin/linux-amd64/keryx-agent-api
```

Check the model catalog with `curl.exe http://127.0.0.1:8787/v1/models` on Windows or `curl http://127.0.0.1:8787/v1/models` on Linux.

| Server variable | Default and usage |
| --- | --- |
| `KERYX_NETWORK` | `mainnet`. Set it to `testnet-10` for testnet. |
| `KERYX_RPC_URL` | `ws://127.0.0.1:24110` on mainnet and `ws://127.0.0.1:24210` on testnet-10. Use the JSON wRPC port. |
| `KERYX_API_PORT` | `8787`. Update the Caddyfile if you change this port. |
| `KERYX_API_DATA_DIR` | `keryx-relay-mainnet` or `keryx-relay-testnet-10`. The database persists request tracking across restarts. |
| `KERYX_TRUST_CADDY` | `0`. Set it to `1` with the supplied Caddyfile so the relay rate-limits by the client IP reported by Caddy. |

## Publish the service over HTTPS

Point your domain or subdomain to the relay host. Install Caddy on that host, allow inbound TCP traffic on ports 80 and 443, and keep the node's wRPC listeners on the local interface. The Caddyfile forwards the client IP, and Caddy manages the HTTPS certificate. [Caddy automatic HTTPS documentation](https://caddyserver.com/docs/automatic-https)

**Windows, in a second PowerShell window**

```powershell
$env:KERYX_DOMAIN = 'api.example.org'
caddy.exe run --config .\Caddyfile --adapter caddyfile
```

**Linux, in a second terminal**

```bash
KERYX_DOMAIN=api.example.org caddy run --config ./Caddyfile --adapter caddyfile
```

Replace `api.example.org` with your domain. From another internet connection, check `https://api.example.org/healthz` and `https://api.example.org/v1/models`. For continuous operation, run the relay and Caddy as host services.

For hosted accounts and the `/mcp` endpoint, follow [MCP.md](MCP.md#set-up-the-hosted-service).

## Call the native API from an agent

| Public route | Purpose |
| --- | --- |
| `GET /v1/models` | List models, eligible miners, and the minimum cost in sompi |
| `POST /v1/requests` | Submit an encrypted, signed Keryx `AiRequest` transaction |
| `GET /v1/requests/{transaction_id}` | Get request status and encrypted Keryx responses |
| `GET /openapi.json` | Get the OpenAPI specification for client integration |

Any integration that can produce the native JSON for a signed `AiRequest` can use the relay. The supplied client uses a trusted Keryx node to read the wallet's unspent transaction outputs (UTXOs) and miners' keys. That node needs `--utxoindex` and can run on a different host from the relay. The agent operator chooses the trusted RPC endpoint.

Run `keryx-agent init` to get the local wallet's public address and fund it on the selected network. Spendable funds must cover the request cost and a change output of at least 1 KRX. The change returns to the wallet. Keep `keryx-wallet-<network>` on the agent machine. It holds the wallet key and the secrets needed to decrypt responses.

**Windows PowerShell**

```powershell
$env:KERYX_API_URL = 'https://api.example.org'
$agent = '.\bin\windows-amd64\keryx-agent.exe'
& $agent init

$promptFile = Join-Path $PWD 'prompt.txt'
[IO.File]::WriteAllText($promptFile, 'Explain the role of Proof-of-Model in two sentences.', [Text.UTF8Encoding]::new($false))
$job = & $agent prepare 0 prompt.txt 80000000 128 | ConvertFrom-Json
& $agent submit $job.transaction_id
& $agent result $job.transaction_id
```

**Linux, after building the client**

```bash
export KERYX_API_URL=https://api.example.org
agent=./agent/target/release/keryx-agent
"$agent" init

printf '%s\n' 'Explain the role of Proof-of-Model in two sentences.' > prompt.txt
"$agent" prepare 0 prompt.txt 80000000 128
"$agent" submit TRANSACTION_ID
"$agent" result TRANSACTION_ID
```

In the Linux commands, replace `TRANSACTION_ID` with the ID returned by `prepare`. The example sets a maximum cost of 80,000,000 sompi, or 0.8 KRX. The `prepare` command encrypts, signs, and saves the transaction locally. The `submit` command broadcasts it. The `result` command displays the decrypted response once available. If the submission outcome is uncertain, retry `submit` with the **same transaction ID** to reuse the signed transaction.

HTTP integrations can run `keryx-agent export TRANSACTION_ID` and POST the resulting JSON to `/v1/requests`. The export contains the network and the transaction. It can be broadcast as-is, and the local request state provides the keys needed for decryption.

| Client variable | Default and usage |
| --- | --- |
| `KERYX_NETWORK` | `mainnet`. Use the same network as the relay. |
| `KERYX_API_URL` | `http://127.0.0.1:8787`. Use the public HTTPS URL for a remote API. |
| `KERYX_AGENT_RPC_URL` | `ws://127.0.0.1:23110` on mainnet and `ws://127.0.0.1:23210` on testnet-10 when using Borsh. |
| `KERYX_AGENT_RPC_ENCODING` | `borsh`. Set it to `json` when connecting to a JSON wRPC listener. |
| `KERYX_AGENT_DATA_DIR` | `keryx-wallet-<network>`. |

## Request tracking and limits

HTTP `202` means the relay processed the submission. The `broadcast_unconfirmed` status means the node may have received the transaction. The `submitted` status means the node saw it. The relay uses the virtual chain and a ten-confirmation threshold for `confirmed` and `answered`. Keryx audits follow their own cycle.

The relay tracks up to 4,096 requests. It tracks an accepted request for 48 hours and an unconfirmed submission for one hour. The node must retain the blocks needed for tracking. Wait at least three seconds between result checks. The relay rate-limits requests by IP address and returns `429` or `503` when limits are reached or capacity is exhausted.

## Run local tests and builds

Run each command block from the repository root. The relay requires Go 1.26 or later. The tests include a simulated HTTP exchange with a transaction signed by the Keryx libraries and a response encrypted by the same libraries.

```text
cd relay
go test -mod=readonly ./...
go build -mod=readonly -trimpath .
```

The wallet component builds on Windows and Linux with Rust 1.96 or later. It uses the Keryx `v1.6.7-PoM` libraries and downloads dependencies from GitHub and crates.io.

```text
cd agent
cargo build --locked --release
cargo test --locked
```

The relay and wallet component passed their builds and tests on Windows and Ubuntu. The inference submission and result tests use simulated data. A live request with a funded account and an active miner still needs to be validated. [Windows and Ubuntu test results](https://github.com/izzback831-cell/keryx-agent-api/actions/runs/45592585847)

- [Keryx inference protocol and encryption](https://keryx-labs.com/whitepaper)
- [Keryx node version used as the protocol reference](https://github.com/Keryx-Labs/keryx-node/releases/tag/v1.6.8-PoM)
