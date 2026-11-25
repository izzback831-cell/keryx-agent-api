use crate::{Config, LocalRequest, RequestInput, store::Wallet};
use anyhow::{Context, Result, anyhow, bail, ensure};
use keryx_addresses::Address;
use keryx_consensus_core::{
    collateral::{private_cohort_rank, private_cohort_seed, private_target_cohort},
    config::params::{INFERENCE_REWARD_MINIMUMS_FLAT, Params, safe_prompt_len, service_tiers},
    constants::{SOMPI_PER_KASPA, TX_VERSION},
    sign::{Signed, sign_with_multiple_v2},
    subnets::SUBNETWORK_ID_AI_REQUEST,
    tx::{
        MutableTransaction, ScriptPublicKey, Transaction, TransactionInput, TransactionOutpoint,
        TransactionOutput, UtxoEntry,
    },
};
use keryx_inference::{
    INFERENCE_VAULT_SCRIPT, MAX_PRIVATE_RECIPIENTS, MIN_AI_REQUEST_PRIORITY_FEE, seal_request,
};
use keryx_rpc_core::{
    GetServerInfoResponse, GetServiceProvidersResponse, RpcHash, RpcResult,
    RpcUtxosByAddressesEntry,
};
use keryx_txscript::pay_to_address_script;
use keryx_wrpc_client::prelude::{
    ConnectOptions, ConnectStrategy, KaspaRpcClient, NetworkId, RpcApi,
};
use std::{
    collections::{BTreeSet, HashSet},
    future::Future,
    time::Duration,
};
use tokio::time::timeout;
use zeroize::Zeroizing;

pub(crate) type Outpoints = HashSet<(RpcHash, u32)>;

const RPC_TIMEOUT: Duration = Duration::from_secs(8);
const REORG_BUFFER_DAA: u64 = 10;
const SEAL_WINDOW_DAA: u64 = 18_000;
const MAX_INPUTS: usize = 32;
const MIN_CHANGE: u64 = SOMPI_PER_KASPA;

pub(crate) struct Node {
    rpc: KaspaRpcClient,
    network: NetworkId,
    wallet: Wallet,
}

impl Node {
    pub(crate) async fn connect(config: &Config, wallet: Wallet) -> Result<Self> {
        let rpc = KaspaRpcClient::new(
            config.encoding,
            Some(&config.rpc_url),
            None,
            Some(config.network),
            None,
        )?;
        timeout(
            RPC_TIMEOUT,
            rpc.connect(Some(ConnectOptions {
                strategy: ConnectStrategy::Fallback,
                ..Default::default()
            })),
        )
        .await
        .context("could not connect to the trusted Keryx RPC before the timeout")??;
        Ok(Self {
            rpc,
            network: config.network,
            wallet,
        })
    }

    pub(crate) async fn prepare(
        &self,
        input: &RequestInput,
        reserved: &Outpoints,
    ) -> Result<LocalRequest> {
        let model = service_tiers(true)
            .get(usize::from(input.tier))
            .context("choose a model tier from 0 to 4")?
            .model_id;
        ensure!(
            (1..=4096).contains(&input.max_tokens),
            "max_tokens must be between 1 and 4096"
        );
        let max_prompt = safe_prompt_len(input.tier, input.max_tokens).unwrap_or_default() as usize;
        ensure!(
            !input.prompt.trim().is_empty() && input.prompt.len() <= max_prompt,
            "the prompt must be non-empty and fit the model's context"
        );
        let reward = INFERENCE_REWARD_MINIMUMS_FLAT
            .iter()
            .find(|(id, _)| *id == model)
            .context("model reward is unavailable")?
            .1;
        let fee = MIN_AI_REQUEST_PRIORITY_FEE;
        ensure!(
            reward + fee <= input.max_cost,
            "the request would cost {} sompi, above the specified budget",
            reward + fee
        );
        let info = call(self.rpc.get_server_info()).await?;
        ensure!(
            info.network_id == self.network,
            "the trusted node belongs to another Keryx network"
        );
        let params = Params::from(self.network);
        ensure!(
            info.is_synced
                && params
                    .verifiable_inference_activation
                    .is_active(info.virtual_daa_score),
            "the trusted node must be synchronized past H15"
        );
        ensure!(info.has_utxo_index, "the trusted node needs --utxoindex");
        let providers = call(self.rpc.get_service_providers(None)).await?;
        let current = escrow_keys(&providers, input.tier, &model);
        ensure!(
            !current.is_empty(),
            "no eligible miner currently serves this model"
        );
        let selected = self
            .select_inputs(&info, reserved, reward + fee + MIN_CHANGE)
            .await?;
        let recent = call(self.rpc.get_service_providers(Some(SEAL_WINDOW_DAA))).await?;
        let seed = private_cohort_seed(&TransactionOutpoint::from(selected[0].outpoint.clone()));
        let cohort = current
            .into_iter()
            .map(RpcHash::from_bytes)
            .collect::<Vec<_>>();
        let mut recipients = private_target_cohort(&seed, &cohort)
            .into_iter()
            .map(|hash| hash.as_bytes())
            .collect::<Vec<_>>();
        let mut extra = escrow_keys(&recent, input.tier, &model)
            .into_iter()
            .filter(|key| !recipients.contains(key))
            .collect::<Vec<_>>();
        extra.sort_by_cached_key(|key| (private_cohort_rank(&seed, key), *key));
        extra.truncate(MAX_PRIVATE_RECIPIENTS.saturating_sub(recipients.len()));
        recipients.extend(extra);
        let (payload, secret) = seal_request(
            model,
            input.max_tokens,
            reward,
            fee,
            input.prompt.as_bytes(),
            &recipients,
        )?;
        let tx = sign_request(
            &self.wallet.address,
            &self.wallet.key,
            selected,
            reward,
            fee,
            payload.serialize(),
        )?;
        Ok(LocalRequest {
            network: self.network.to_string(),
            transaction_id: tx.id(),
            transaction: (&tx).into(),
            secret: Zeroizing::new(secret.to_hex()),
            cost_sompi: reward + fee,
            prepare_key: None,
            prepare_fingerprint: None,
        })
    }

    async fn select_inputs(
        &self,
        info: &GetServerInfoResponse,
        reserved: &Outpoints,
        required: u64,
    ) -> Result<Vec<RpcUtxosByAddressesEntry>> {
        let mut available = call(
            self.rpc
                .get_utxos_by_addresses(vec![self.wallet.address.clone()]),
        )
        .await?;
        let params = Params::from(self.network);
        let script = pay_to_address_script(&self.wallet.address);
        available.retain(|entry| {
            let utxo = &entry.utxo_entry;
            let maturity = if utxo.is_coinbase {
                params.coinbase_maturity()
            } else {
                REORG_BUFFER_DAA
            };
            utxo.block_daa_score.saturating_add(maturity) <= info.virtual_daa_score
                && utxo.script_public_key == script
                && !reserved.contains(&(entry.outpoint.transaction_id, entry.outpoint.index))
        });
        available.sort_by_key(|entry| std::cmp::Reverse(entry.utxo_entry.amount));
        let mut total = 0u64;
        let mut selected = Vec::new();
        for entry in available.into_iter().take(MAX_INPUTS) {
            total = total
                .checked_add(entry.utxo_entry.amount)
                .context("wallet balance overflow")?;
            selected.push(entry);
            if total >= required {
                return Ok(selected);
            }
        }
        bail!("wallet needs {required} spendable sompi, including change")
    }
}

fn sign_request(
    address: &Address,
    wallet_key: &[u8; 32],
    utxos: Vec<RpcUtxosByAddressesEntry>,
    reward: u64,
    fee: u64,
    payload: Vec<u8>,
) -> Result<Transaction> {
    let inputs = utxos
        .iter()
        .map(|entry| TransactionInput::new(entry.outpoint.clone().into(), vec![], 0, 1))
        .collect();
    let total = utxos
        .iter()
        .map(|entry| entry.utxo_entry.amount)
        .sum::<u64>();
    let outputs = vec![
        TransactionOutput::new(total - reward - fee, pay_to_address_script(address)),
        TransactionOutput::new(
            reward,
            ScriptPublicKey::new(0, INFERENCE_VAULT_SCRIPT.to_vec().into()),
        ),
    ];
    let entries = utxos
        .into_iter()
        .map(|entry| UtxoEntry::from(entry.utxo_entry))
        .collect();
    let tx = Transaction::new_non_finalized(
        TX_VERSION,
        inputs,
        outputs,
        0,
        SUBNETWORK_ID_AI_REQUEST,
        0,
        payload,
    );
    let keys = Zeroizing::new([*wallet_key]);
    let Signed::Fully(mut signed) =
        sign_with_multiple_v2(MutableTransaction::with_entries(tx, entries), &*keys)
    else {
        bail!("could not sign every input with the local wallet");
    };
    signed.tx.finalize();
    Ok(signed.tx)
}

fn escrow_keys(
    providers: &GetServiceProvidersResponse,
    tier: u8,
    model_id: &[u8; 32],
) -> BTreeSet<[u8; 32]> {
    providers
        .providers
        .iter()
        .filter(|provider| {
            provider.tier == u32::from(tier) && &provider.model_id.as_bytes() == model_id
        })
        .map(|provider| provider.escrow_pubkey.as_bytes())
        .collect()
}

async fn call<T>(future: impl Future<Output = RpcResult<T>>) -> Result<T> {
    timeout(RPC_TIMEOUT, future)
        .await
        .context("the trusted Keryx RPC timed out")?
        .map_err(|error| anyhow!("trusted Keryx RPC: {error}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{RelayStatus, decrypt};
    use keryx_addresses::Version;
    use keryx_consensus_core::{
        sign::verify, subnets::SUBNETWORK_ID_AI_RESPONSE, tx::PopulatedTransaction,
    };
    use keryx_inference::{
        AiRequestPayload, AiResponder, AiResponsePayload, escrow_pubkey_of, open_request,
        seal_request_with_rng, seal_response,
    };
    use keryx_rpc_core::{RpcTransactionOutpoint, RpcUtxoEntry};
    use keryx_wrpc_client::prelude::NetworkType;
    use rand::{SeedableRng, rngs::StdRng};
    use serde_json::json;

    #[test]
    fn signs_a_native_request_and_decrypts_a_relayed_answer() {
        let network = NetworkId::new(NetworkType::Mainnet);
        let funding_key = [6; 32];
        let miner_key = [7; 32];
        let address = Address::new(
            network.into(),
            Version::PubKey,
            &escrow_pubkey_of(&funding_key).unwrap(),
        );
        let escrow = escrow_pubkey_of(&miner_key).unwrap();
        let reward = 50_000_000;
        let fee = MIN_AI_REQUEST_PRIORITY_FEE;
        let model = service_tiers(true)[0].model_id;
        let (payload, secret) = seal_request_with_rng(
            model,
            100,
            reward,
            fee,
            b"Keryx replay fixture",
            &[escrow],
            &mut StdRng::seed_from_u64(8),
        )
        .unwrap();
        let funding = RpcUtxoEntry::new(
            reward + fee + MIN_CHANGE,
            pay_to_address_script(&address),
            1,
            false,
        );
        let entry = RpcUtxosByAddressesEntry {
            address: Some(address.clone()),
            outpoint: RpcTransactionOutpoint {
                transaction_id: RpcHash::from_bytes([1; 32]),
                index: 7,
            },
            utxo_entry: funding.clone(),
        };
        let tx = sign_request(
            &address,
            &funding_key,
            vec![entry],
            reward,
            fee,
            payload.serialize(),
        )
        .unwrap();
        verify(&PopulatedTransaction {
            tx: &tx,
            entries: vec![funding.into()],
        })
        .unwrap();
        let request = LocalRequest {
            network: network.to_string(),
            transaction_id: tx.id(),
            transaction: (&tx).into(),
            secret: Zeroizing::new(secret.to_hex()),
            cost_sompi: reward + fee,
            prepare_key: None,
            prepare_fingerprint: None,
        };
        let opened = open_request(
            &AiRequestPayload::deserialize(&request.transaction.payload).unwrap(),
            &miner_key,
        )
        .unwrap();
        assert_eq!(opened.prompt, b"Keryx replay fixture");
        let block = RpcHash::from_bytes([2; 32]);
        let answer_id = RpcHash::from_bytes([3; 32]);
        let encrypted = seal_response(
            &opened.response_key,
            &tx.id().as_bytes(),
            b"Replayed Keryx answer",
        );
        let response = AiResponsePayload::new_v2(
            tx.id().as_bytes(),
            0,
            [0; 34],
            4,
            AiResponder {
                escrow_pubkey: escrow,
                signature: [0; 64],
            },
        )
        .with_private_body(encrypted.clone());
        let status: RelayStatus = serde_json::from_value(json!({
            "network": network, "transaction_id": tx.id(), "state": "answered",
            "answers": [{
                "transaction_id": answer_id, "accepting_block": block, "escrow_key": hex::encode(escrow),
                "encrypted_body": hex::encode(&encrypted),
            }],
        })).unwrap();
        assert_eq!(
            decrypt(&request, &status).unwrap().unwrap().text,
            "Replayed Keryx answer"
        );
        if let Some(path) = std::env::var_os("KERYX_TEST_VECTOR") {
            let vector = json!({
                "submit": request.public(), "id": request.transaction_id, "answer_id": answer_id,
                "escrow": hex::encode(escrow), "encrypted": hex::encode(&encrypted),
                "chain": {
                    "removedChainBlockHashes": [],
                    "addedChainBlockHashes": [block],
                    "chainBlockAcceptedTransactions": [{
                        "acceptedTransactions": [
                            {"verboseData":{"transactionId": tx.id()}, "subnetworkId": SUBNETWORK_ID_AI_REQUEST},
                            {"verboseData":{"transactionId": answer_id}, "subnetworkId": SUBNETWORK_ID_AI_RESPONSE, "payload": response.serialize()},
                        ],
                    }],
                }
            });
            std::fs::write(path, serde_json::to_vec_pretty(&vector).unwrap()).unwrap();
        }
    }
}
