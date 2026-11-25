# Connexion des assistants à Keryx

L’utilisateur ajoute l’URL HTTPS du service et sa clé API dans son assistant. Le serveur héberge le nœud Keryx, prépare les transactions et restitue les réponses. Le service expose Keryx comme un outil que l’assistant peut appeler depuis Claude Code, Codex, VS Code avec Copilot, Cursor, Cline et Gemini CLI.

Chaque compte dispose d’un portefeuille Keryx dédié, hébergé et administré par l’opérateur. Le prompt et la réponse en clair sont traités sur ce serveur. Le nœud et les mineurs utilisent ensuite le chiffrement prévu par Keryx. Le mode hébergé utilise les KRX déposés à l’adresse du compte. Un KRX vaut 100 000 000 sompi.

L’opérateur remet deux éléments à l’utilisateur, l’URL se terminant par `/mcp` et une clé API. L’utilisateur configure cette clé dans son environnement sous le nom `KERYX_API_KEY`. Dans les exemples suivants, remplace `https://api.example.org/mcp` par l’URL du service. Ces fichiers sont des exemples à fusionner avec les réglages existants de l’application.

| Assistant | Configuration fournie | Destination |
| --- | --- | --- |
| Claude Code | `configs/claude-code.json` | `.mcp.json` du projet ou ajout par `claude mcp add` |
| Codex CLI et extension | `configs/codex.toml` | `~/.codex/config.toml` |
| VS Code avec Copilot | `configs/copilot.json` | `~/.copilot/mcp-config.json` ou `.mcp.json` du projet |
| VS Code avec le client MCP de l’éditeur | `configs/vscode.json` | `.vscode/mcp.json` |
| Cursor | `configs/cursor.json` | `~/.cursor/mcp.json` ou `.cursor/mcp.json` du projet |
| Cline | `configs/cline.json` | Paramètres MCP de Cline, onglet des serveurs distants |
| Gemini CLI | `configs/gemini.json` | `~/.gemini/settings.json` |

Après la connexion, demande à l’assistant « Liste les modèles Keryx disponibles et leur coût ». Il appelle `keryx_models`. Pour vérifier l’adresse à financer et les limites, demande « Affiche mon compte Keryx ». Les dépôts doivent être effectués sur le réseau indiqué par ce compte.

Pour une inférence, tu peux demander « Utilise Keryx pour obtenir un deuxième avis sur cette fonction. Choisis le tier 0, limite la sortie à 256 tokens et le coût à 0,8 KRX ». L’assistant appelle `keryx_ask`, conserve l’identifiant reçu, puis appelle `keryx_result` au moins trois secondes plus tard. Une réponse prend le temps de son traitement par le réseau.

| Outil | Fonction |
| --- | --- |
| `keryx_models` | Modèles, disponibilité et coût minimal en sompi |
| `keryx_account` | Adresse de financement, réseau et limites du compte |
| `keryx_ask` | Demande d’inférence avec `prompt`, `tier`, `request_key`, `max_cost_sompi` et `max_tokens` facultatif |
| `keryx_result` | État et réponse déchiffrée d’une demande du compte |

La valeur `request_key` identifie une intention de requête. L’assistant réutilise exactement cette valeur et les mêmes paramètres si une réponse HTTP est interrompue. La transaction signée reste la même. Une nouvelle clé correspond à une nouvelle demande payante. Les montants réservés sont comptabilisés sur le jour UTC de création, y compris les préparations encore en attente. Le portefeuille conserve les entrées déjà signées pour empêcher leur réutilisation concurrente.

Les connexions utilisent MCP Streamable HTTP avec réponses JSON et négociation des versions 2025-03-26, 2025-06-18 et 2025-11-25. Les réponses longues se récupèrent par appels successifs à `keryx_result`. Les exemples suivent les documentations officielles de chaque client. L’authentification, les appels d’outils, les reprises et l’isolation des comptes ont été testés par des échanges MCP simulés. La validation dans chaque application et sur un réseau Keryx avec mineurs actifs reste à faire.

## Mise en service par l’opérateur

Sous Windows, les deux exécutables de `bin/windows-amd64` doivent rester ensemble. Sous Linux, le binaire du relais est fourni et le composant portefeuille `keryx-agent` se compile avec `build-linux.sh`. Ce script nécessite Go 1.26+, Rust 1.96+, un compilateur C et l’accès à GitHub et aux registres de dépendances. Lance le script depuis le dossier extrait.

```bash
sh ./build-linux.sh
```

Sur la machine d’hébergement, le nœud Keryx doit être synchronisé avec `--utxoindex`, un port wRPC JSON pour le relais et un port wRPC Borsh pour le portefeuille. Sur mainnet, les ports par défaut sont `24110` pour JSON et `23110` pour Borsh. Sur testnet-10, ils sont respectivement `24210` et `23210`. Les variables `KERYX_RPC_URL`, `KERYX_AGENT_RPC_URL` et `KERYX_AGENT_RPC_ENCODING` permettent d’adapter ces connexions. Le nœud reste sur le réseau de l’hébergeur.

L’exemple crée un compte avec un plafond de 3 KRX par requête et de 20 KRX par jour. La commande affiche une clé API à remettre au titulaire et son adresse publique de financement. Choisis un nom de compte composé de lettres minuscules, de chiffres et de tirets.

```powershell
$env:KERYX_API_DATA_DIR = Join-Path $PWD 'keryx-service-mainnet'
.\bin\windows-amd64\keryx-agent-api.exe account-create utilisateur1 300000000 2000000000
$env:KERYX_TRUST_CADDY = '1'
.\bin\windows-amd64\keryx-agent-api.exe
```

```bash
export KERYX_API_DATA_DIR="$PWD/keryx-service-mainnet"
./bin/linux-amd64/keryx-agent-api account-create utilisateur1 300000000 2000000000
KERYX_TRUST_CADDY=1 ./bin/linux-amd64/keryx-agent-api
```

Le portefeuille doit contenir les KRX du coût et au moins 1 KRX de monnaie disponible. La sortie de monnaie revient au compte. Le titulaire pourra consulter son adresse via `keryx_account`. Les clés privées se trouvent dans le dossier `wallets` de l’hébergeur. Conserve le dossier du service pour préserver les portefeuilles et les demandes après redémarrage. Le fichier `accounts/<nom>.json` contient le hachage de la clé API et les plafonds. Le champ `disabled` permet à l’opérateur de désactiver un accès.

Pour la mise en ligne, configure le domaine dans le Caddyfile déjà fourni et lance Caddy selon le README. L’URL MCP publique sera `https://TON_DOMAINE/mcp`. Pour des appels provenant d’un navigateur, renseigne les origines autorisées, séparées par des virgules, dans `KERYX_MCP_ORIGINS`. Les applications MCP de bureau envoient habituellement leurs appels sans en-tête Origin.

Le même compte peut utiliser l’API HTTP avec `Authorization: Bearer CLE_API`.

| Route | Fonction |
| --- | --- |
| `GET /v1/account` | Informations du compte |
| `POST /v1/agent/requests` | Corps JSON avec `request_key`, `prompt`, `tier`, `max_cost_sompi` et éventuellement `max_tokens` |
| `GET /v1/agent/requests/{request_id}` | Résultat de la demande du compte |

Le serveur conserve jusqu’à 4 096 intentions de requêtes hébergées pour préserver leurs identifiants de reprise. Il refuse les nouvelles intentions lorsque cette capacité est atteinte. Les données de suivi du relais natif gardent leur fenêtre de 48 heures après acceptation. L’archive contient les sources et les tests pour faire évoluer ces limites selon l’usage du service.

## Références des clients

- [Claude Code et MCP](https://code.claude.com/docs/en/mcp)
- [Codex et MCP](https://developers.openai.com/codex/mcp)
- [VS Code et Copilot](https://code.visualstudio.com/docs/agents/reference/mcp-configuration)
- [Cursor et MCP](https://cursor.com/docs/mcp)
- [Cline et MCP](https://docs.cline.bot/mcp/mcp-overview)
- [Gemini CLI et MCP](https://geminicli.com/docs/tools/mcp-server/)
