mod node;
mod store;

use anyhow::{Context, Result, bail, ensure};
use keryx_inference::{PrivateRequestSecret, open_response};
use keryx_rpc_core::{RpcHash, RpcTransaction};
use keryx_wrpc_client::prelude::{NetworkId, NetworkType, WrpcEncoding};
use reqwest::{Client, Response, Url};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use std::{env, fs::File, io::Read, path::PathBuf, time::Duration};
use zeroize::Zeroizing;

use node::Node;
use store::Store;

const USAGE: &str = "keryx-agent init\nkeryx-agent prepare TIER PROMPT_FILE MAX_COST_SOMPI [MAX_TOKENS]\nkeryx-agent export TRANSACTION_ID\nkeryx-agent submit TRANSACTION_ID\nkeryx-agent result TRANSACTION_ID";

struct Config {
    network: NetworkId,
    data_dir: PathBuf,
    rpc_url: String,
    encoding: WrpcEncoding,
}

impl Config {
    fn load() -> Result<Self> {
        let (network, default_borsh, default_json) =
            match env::var("KERYX_NETWORK").as_deref().unwrap_or("mainnet") {
                "mainnet" => (NetworkId::new(NetworkType::Mainnet), 23110, 24110),
                "testnet-10" => (
                    NetworkId::with_suffix(NetworkType::Testnet, 10),
                    23210,
                    24210,
                ),
                _ => bail!("KERYX_NETWORK must be mainnet or testnet-10"),
            };
        let (encoding, port) = match env::var("KERYX_AGENT_RPC_ENCODING")
            .as_deref()
            .unwrap_or("borsh")
        {
            "borsh" => (WrpcEncoding::Borsh, default_borsh),
            "json" => (WrpcEncoding::SerdeJson, default_json),
            _ => bail!("KERYX_AGENT_RPC_ENCODING must be borsh or json"),
        };
        let rpc_url =
            env::var("KERYX_AGENT_RPC_URL").unwrap_or_else(|_| format!("ws://127.0.0.1:{port}"));
        let data_dir = env::var_os("KERYX_AGENT_DATA_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from(format!("keryx-wallet-{network}")));
        Ok(Self {
            network,
            data_dir,
            rpc_url,
            encoding,
        })
    }
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RequestInput {
    tier: u8,
    prompt: Zeroizing<String>,
    max_tokens: u32,
    #[serde(rename = "max_cost_sompi")]
    max_cost: u64,
}

#[derive(Serialize, Deserialize)]
pub(crate) struct LocalRequest {
    network: String,
    transaction_id: RpcHash,
    transaction: RpcTransaction,
    secret: Zeroizing<String>,
    cost_sompi: u64,
    #[serde(default)]
    prepare_key: Option<String>,
    #[serde(default)]
    prepare_fingerprint: Option<String>,
}

#[derive(Serialize)]
struct PublicRequest<'a> {
    network: &'a str,
    transaction: &'a RpcTransaction,
}

impl LocalRequest {
    fn public(&self) -> PublicRequest<'_> {
        PublicRequest {
            network: &self.network,
            transaction: &self.transaction,
        }
    }
}

#[derive(Deserialize)]
struct RelayStatus {
    network: String,
    transaction_id: RpcHash,
    state: String,
    answers: Vec<EncryptedAnswer>,
}

#[derive(Deserialize)]
struct EncryptedAnswer {
    transaction_id: RpcHash,
    accepting_block: RpcHash,
    escrow_key: RpcHash,
    #[serde(with = "hex::serde")]
    encrypted_body: Vec<u8>,
}

#[derive(Serialize)]
struct Answer {
    text: String,
    transaction_id: String,
    accepting_block: String,
}

fn decrypt(request: &LocalRequest, status: &RelayStatus) -> Result<Option<Answer>> {
    let secret = PrivateRequestSecret::from_hex(&request.secret)?;
    for answer in &status.answers {
        if let Ok(body) = open_response(
            &secret,
            &request.transaction_id.as_bytes(),
            &answer.escrow_key.as_bytes(),
            &answer.encrypted_body,
        ) {
            return Ok(Some(Answer {
                text: String::from_utf8_lossy(&body).into_owned(),
                transaction_id: answer.transaction_id.to_string(),
                accepting_block: answer.accepting_block.to_string(),
            }));
        }
    }
    Ok(None)
}

fn relay_base() -> Result<String> {
    let raw = env::var("KERYX_API_URL").unwrap_or_else(|_| "http://127.0.0.1:8787".to_owned());
    let url = Url::parse(&raw).context("invalid KERYX_API_URL")?;
    let local = matches!(url.host_str(), Some("127.0.0.1" | "localhost" | "[::1]"));
    ensure!(
        url.scheme() == "https" || (local && url.scheme() == "http"),
        "use an HTTPS relay URL or a local HTTP relay"
    );
    ensure!(
        url.host_str().is_some()
            && url.username().is_empty()
            && url.password().is_none()
            && url.query().is_none()
            && url.fragment().is_none(),
        "the relay URL must contain a host and a base path"
    );
    Ok(raw.trim_end_matches('/').to_owned())
}

async fn response_status(mut response: Response, request: &LocalRequest) -> Result<RelayStatus> {
    let code = response.status();
    let mut bytes = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        ensure!(
            bytes.len() + chunk.len() <= 2 << 20,
            "relay response exceeded the 2 MiB limit"
        );
        bytes.extend_from_slice(&chunk);
    }
    if !code.is_success() {
        let error: serde_json::Value = serde_json::from_slice(&bytes).unwrap_or_default();
        let label = error["error"]["code"].as_str().unwrap_or("relay_error");
        bail!("HTTP {code} from relay ({label})");
    }
    let status: RelayStatus = serde_json::from_slice(&bytes)?;
    ensure!(
        status.network == request.network && status.transaction_id == request.transaction_id,
        "the relay replied with a different transaction or network"
    );
    Ok(status)
}

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<_> = env::args().skip(1).collect();
    if args.is_empty() || matches!(args[0].as_str(), "-h" | "--help") {
        println!("{USAGE}");
        return Ok(());
    }
    if args[0] == "--version" {
        println!("Keryx agent 0.3.0");
        return Ok(());
    }
    let config = Config::load()?;
    let store = Store::open(&config.data_dir)?;
    match args[0].as_str() {
        "init" if args.len() == 1 => {
            let wallet = store.wallet(config.network, true)?;
            println!(
                "{}",
                json!({"network": config.network.to_string(), "funding_address": wallet.address.to_string()})
            );
        }
        "prepare-job" if args.len() == 2 => {
            let key = &args[1];
            ensure!(
                key.len() == 64 && key.bytes().all(|b| b.is_ascii_hexdigit()),
                "expected a 64-character preparation key"
            );
            let mut bytes = Zeroizing::new(Vec::new());
            std::io::stdin().take(262_145).read_to_end(&mut bytes)?;
            ensure!(bytes.len() <= 262_144, "input too large");
            let input: RequestInput = serde_json::from_slice(&bytes)?;
            let canonical = Zeroizing::new(serde_json::to_vec(&input)?);
            let fingerprint = hex::encode(Sha256::digest(&canonical));
            let request = match store.prepared(key)? {
                Some(saved) => {
                    ensure!(
                        saved.prepare_fingerprint.as_ref() == Some(&fingerprint),
                        "the preparation key belongs to a different request"
                    );
                    saved
                }
                None => {
                    let wallet = store.wallet(config.network, false)?;
                    let reserved = store.reserved(&config.network.to_string())?;
                    let node = Node::connect(&config, wallet).await?;
                    let mut request = node.prepare(&input, &reserved).await?;
                    request.prepare_key = Some(key.clone());
                    request.prepare_fingerprint = Some(fingerprint);
                    store.save(&request)?;
                    request
                }
            };
            ensure!(
                request.network == config.network.to_string(),
                "the saved request belongs to a different network"
            );
            println!(
                "{}",
                json!({"network": request.network, "transaction_id": request.transaction_id.to_string(), "cost_sompi": request.cost_sompi})
            );
        }
        "decode" if args.len() == 2 => {
            let request = store.get(&args[1])?;
            let mut bytes = Vec::new();
            std::io::stdin()
                .take((2 << 20) + 1)
                .read_to_end(&mut bytes)?;
            ensure!(bytes.len() <= 2 << 20, "input too large");
            let status: RelayStatus = serde_json::from_slice(&bytes)?;
            ensure!(
                status.network == request.network
                    && status.transaction_id == request.transaction_id,
                "unexpected network or request ID"
            );
            println!(
                "{}",
                json!({"transaction_id": status.transaction_id.to_string(), "state": status.state, "answer": decrypt(&request, &status)?})
            );
        }
        "prepare" if (4..=5).contains(&args.len()) => {
            let mut prompt = Zeroizing::new(String::new());
            File::open(&args[2])
                .context("open the prompt file")?
                .take(65_537)
                .read_to_string(&mut prompt)?;
            ensure!(prompt.len() <= 65_536, "prompt file exceeds 64 KiB");
            let input = RequestInput {
                tier: args[1].parse().context("invalid model tier")?,
                prompt,
                max_cost: args[3].parse().context("invalid spending cap in sompi")?,
                max_tokens: args
                    .get(4)
                    .map_or(Ok(256), |arg| arg.parse().context("invalid token limit"))?,
            };
            let wallet = store.wallet(config.network, false)?;
            let reserved = store.reserved(&config.network.to_string())?;
            let node = Node::connect(&config, wallet).await?;
            let request = node.prepare(&input, &reserved).await?;
            store.save(&request)?;
            println!(
                "{}",
                json!({"network": request.network, "transaction_id": request.transaction_id.to_string(), "cost_sompi": request.cost_sompi})
            );
        }
        command @ ("export" | "submit" | "result") if args.len() == 2 => {
            let request = store.get(&args[1])?;
            ensure!(
                request.network == config.network.to_string(),
                "select the network of the local request"
            );
            if command == "export" {
                println!("{}", serde_json::to_string(&request.public())?);
                return Ok(());
            }
            let base = relay_base()?;
            let client = Client::builder()
                .timeout(Duration::from_secs(28))
                .redirect(reqwest::redirect::Policy::none())
                .build()?;
            let reply = if command == "submit" {
                client
                    .post(format!("{base}/v1/requests"))
                    .json(&request.public())
                    .send()
                    .await?
            } else {
                client
                    .get(format!("{base}/v1/requests/{}", request.transaction_id))
                    .send()
                    .await?
            };
            let status = response_status(reply, &request).await?;
            let answer = decrypt(&request, &status)?;
            println!(
                "{}",
                json!({
                    "network": status.network, "transaction_id": status.transaction_id.to_string(), "state": status.state,
                    "answer": answer,
                })
            );
        }
        _ => bail!("{USAGE}"),
    }
    Ok(())
}
