# Keryx Agent API 0.3

API publique et serveur MCP pour les demandes d’inférence Keryx. Les utilisateurs de Claude Code, Codex, VS Code/Copilot, Cursor, Cline et Gemini CLI se connectent par URL HTTPS et clé API. Le mode hébergé prépare les transactions et gère un portefeuille dédié par compte sur le serveur. Consulte [MCP.md](MCP.md) pour ce fonctionnement et les configurations prêtes à adapter.

Les routes natives `/v1/requests` continuent de recevoir des transactions déjà signées. La suite de ce guide décrit le relais natif, qui garde le portefeuille sur la machine de l’agent. La configuration du domaine et du nœud côté hébergement est commune aux deux accès.

| Fichier | Usage |
| --- | --- |
| `bin/windows-amd64/keryx-agent-api.exe` | Serveur API Windows 64 bits |
| `bin/linux-amd64/keryx-agent-api` | Serveur API Linux x86-64 |
| `bin/windows-amd64/keryx-agent.exe` | Client de préparation, soumission et lecture sous Windows |
| `agent/` | Sources du client pour Windows et Linux |
| `relay/` | Sources du serveur et contrat OpenAPI |
| `Caddyfile` | Publication HTTPS derrière Caddy |

## Compiler depuis un clone Git

Installe Go 1.26+, Rust 1.96+ et un compilateur C. Sous Windows x86-64, utilise les outils C++ de Visual Studio pour le linker MSVC. Depuis la racine du dépôt, lance PowerShell.

```powershell
Push-Location relay
go build -mod=readonly -trimpath -o ../bin/windows-amd64/keryx-agent-api.exe .
Pop-Location
cargo build --locked --release --manifest-path agent/Cargo.toml
Copy-Item agent/target/release/keryx-agent.exe bin/windows-amd64/
```

Sous Linux x86-64, lance `sh ./build-linux.sh` depuis la racine du dépôt. Le relais et le composant portefeuille se retrouvent dans `bin/linux-amd64/`.

## Démarrer le serveur

Le réseau principal est sélectionné par défaut. Utilise un nœud Keryx synchronisé qui écoute en wRPC JSON sur `127.0.0.1:24110`. Ajoute `--rpclisten-json=127.0.0.1:24110` à la commande de démarrage du nœud si ce port reste à activer. Le serveur API reçoit ses requêtes HTTP sur `127.0.0.1:8787`.

**Windows**

```powershell
$env:KERYX_TRUST_CADDY = '1'
.\bin\windows-amd64\keryx-agent-api.exe
```

**Linux**

```bash
chmod +x bin/linux-amd64/keryx-agent-api
KERYX_TRUST_CADDY=1 ./bin/linux-amd64/keryx-agent-api
```

Teste le catalogue avec `curl.exe http://127.0.0.1:8787/v1/models` sous Windows ou `curl http://127.0.0.1:8787/v1/models` sous Linux.

| Variable serveur | Valeur par défaut |
| --- | --- |
| `KERYX_NETWORK` | `mainnet`. Pour le test, choisir `testnet-10`. |
| `KERYX_RPC_URL` | `ws://127.0.0.1:24110` sur mainnet et `ws://127.0.0.1:24210` sur testnet. Utiliser le port wRPC JSON. |
| `KERYX_API_PORT` | `8787`. Répercuter tout changement dans le Caddyfile. |
| `KERYX_API_DATA_DIR` | `keryx-relay-mainnet` ou `keryx-relay-testnet-10`. La base conserve le suivi après redémarrage. |
| `KERYX_TRUST_CADDY` | `0`. Mettre `1` avec le Caddyfile fourni pour limiter les appels selon l’adresse du client vue par Caddy. |

## Ouvrir l’accès public en HTTPS

Fais pointer ton domaine ou sous-domaine vers l’hôte du relais. Installe Caddy sur cet hôte, rends les ports TCP 80 et 443 accessibles et garde le wRPC du nœud sur l’interface locale. Le Caddyfile transmet l’adresse du client et laisse Caddy gérer le certificat HTTPS. [Documentation Caddy](https://caddyserver.com/docs/automatic-https)

**Windows, dans un deuxième PowerShell**

```powershell
$env:KERYX_DOMAIN = 'api.example.org'
caddy.exe run --config .\Caddyfile --adapter caddyfile
```

**Linux, dans un deuxième terminal**

```bash
KERYX_DOMAIN=api.example.org caddy run --config ./Caddyfile --adapter caddyfile
```

Remplace `api.example.org` par ton domaine. Depuis une autre connexion Internet, vérifie `https://api.example.org/healthz`, puis `https://api.example.org/v1/models`. Pour une exploitation continue, lance le relais et Caddy comme services de l’hôte.

## Appeler l’API depuis un agent

| Route publique | Fonction |
| --- | --- |
| `GET /v1/models` | Modèles, mineurs éligibles et coût minimal en sompi |
| `POST /v1/requests` | Dépôt d’une transaction Keryx `AiRequest` chiffrée et signée |
| `GET /v1/requests/{transaction_id}` | État et réponses Keryx chiffrées |
| `GET /openapi.json` | Contrat à importer dans le client |

Toute intégration qui sait produire le JSON natif d’une `AiRequest` signée peut utiliser le relais. Le client livré s’appuie sur un nœud Keryx de confiance pour lire les UTXO du portefeuille et les clés des mineurs. Ce nœud doit avoir `--utxoindex` et peut être situé ailleurs que le relais. Le choix du RPC de confiance appartient à l’agent.

`keryx-agent init` donne l’adresse publique du portefeuille local. Alimente cette adresse sur le réseau choisi. Le montant disponible doit couvrir le coût et une sortie de monnaie d’au moins 1 KRX. La monnaie retourne au portefeuille. Conserve le dossier `keryx-wallet-<réseau>` sur la machine de l’agent. Il contient la clé du portefeuille et les secrets nécessaires à la lecture des réponses.

**Windows PowerShell**

```powershell
$env:KERYX_API_URL = 'https://api.example.org'
$agent = '.\bin\windows-amd64\keryx-agent.exe'
& $agent init

$promptFile = Join-Path $PWD 'prompt.txt'
[IO.File]::WriteAllText($promptFile, 'Explique le rôle du Proof-of-Model en deux phrases.', [Text.UTF8Encoding]::new($false))
$job = & $agent prepare 0 prompt.txt 80000000 128 | ConvertFrom-Json
& $agent submit $job.transaction_id
& $agent result $job.transaction_id
```

**Linux, après compilation du client**

```bash
export KERYX_API_URL=https://api.example.org
agent=./agent/target/release/keryx-agent
"$agent" init

printf '%s\n' 'Explique le rôle du Proof-of-Model en deux phrases.' > prompt.txt
"$agent" prepare 0 prompt.txt 80000000 128
"$agent" submit TRANSACTION_ID
"$agent" result TRANSACTION_ID
```

Dans les commandes Linux, remplace `TRANSACTION_ID` par l’identifiant renvoyé par `prepare`. Le coût maximal de l’exemple est de 80 000 000 sompi, soit 0,8 KRX. `prepare` chiffre, signe et enregistre la transaction localement. `submit` diffuse cette transaction. `result` affiche la réponse déchiffrée dès qu’elle est disponible. Réessaie `submit` avec le **même identifiant** si l’envoi est incertain. Cette opération réutilise la transaction signée.

Les intégrations HTTP peuvent lancer `keryx-agent export TRANSACTION_ID` et poster le JSON généré sur `/v1/requests`. L’export contient le réseau et la transaction. Il peut être diffusé tel quel, et le déchiffrement reste associé à la demande locale.

| Variable client | Valeur par défaut |
| --- | --- |
| `KERYX_NETWORK` | `mainnet`. Utiliser le même réseau que le relais. |
| `KERYX_API_URL` | `http://127.0.0.1:8787`. Pour une API distante, configurer son URL HTTPS. |
| `KERYX_AGENT_RPC_URL` | `ws://127.0.0.1:23110` sur mainnet et `ws://127.0.0.1:23210` sur testnet en Borsh. |
| `KERYX_AGENT_RPC_ENCODING` | `borsh`. Choisir `json` en accord avec le port du RPC de confiance. |
| `KERYX_AGENT_DATA_DIR` | `keryx-wallet-<réseau>` |

## Suivi et limites du relais

Le code HTTP `202` indique que le relais a traité la soumission. L’état `broadcast_unconfirmed` indique que la réception par le nœud reste incertaine. `submitted` indique que le nœud a vu la transaction. `confirmed` et `answered` reposent sur la chaîne virtuelle avec un seuil de dix confirmations. Les audits Keryx suivent leur propre cycle.

Le relais conserve jusqu’à 4 096 demandes. Il suit une demande acceptée pendant 48 heures et un envoi sans confirmation pendant une heure. Le nœud doit conserver les blocs nécessaires au suivi. Espace les appels de résultat d’au moins trois secondes. Le relais limite les appels par adresse et renvoie `429` ou `503` en cas de saturation.

## Compiler et vérifier

Relais avec Go 1.26 ou ultérieur. Les tests incluent un échange HTTP simulé utilisant une transaction signée par les bibliothèques Keryx et une réponse chiffrée issue de ces mêmes bibliothèques.

```text
cd relay
go test -mod=readonly ./...
go build -mod=readonly -trimpath .
```

Client sous Windows ou Linux avec Rust 1.96 ou ultérieur. Le client emploie les bibliothèques Keryx `v1.6.7-PoM`. Les dépendances sont récupérées auprès de GitHub et de crates.io.

```text
cd agent
cargo build --locked --release
cargo test --locked
```

Le relais Windows a été lancé et le relais Linux a été compilé pour x86-64. Le test de soumission et de récupération utilise des données d’inférence simulées. La validation sur un nœud synchronisé avec des mineurs actifs reste à faire.

- [Protocole d’inférence et chiffrement Keryx](https://keryx-labs.com/whitepaper)
- [Version de keryxd utilisée comme référence réseau](https://github.com/Keryx-Labs/keryx-node/releases/tag/v1.6.8-PoM)
