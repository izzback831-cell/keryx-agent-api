use crate::{LocalRequest, node::Outpoints};
use anyhow::{Context, Result, ensure};
use keryx_addresses::{Address, Version};
use keryx_inference::escrow_pubkey_of;
use keryx_wrpc_client::prelude::NetworkId;
use rand::{RngCore, rngs::OsRng};
use serde::{Deserialize, Serialize};
use std::{
    fs::{self, DirBuilder, File},
    io::{ErrorKind, Write},
    path::{Path, PathBuf},
};
use tempfile::NamedTempFile;
use zeroize::Zeroizing;

pub(crate) struct Wallet {
    pub key: Zeroizing<[u8; 32]>,
    pub address: Address,
}

#[derive(Serialize, Deserialize, PartialEq)]
struct Profile {
    network: String,
    address: String,
}

pub(crate) struct Store {
    root: PathBuf,
    jobs: PathBuf,
    _lock: File,
}

impl Store {
    pub(crate) fn open(root: &Path) -> Result<Self> {
        let mut builder = DirBuilder::new();
        builder.recursive(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::DirBuilderExt;
            builder.mode(0o700);
        }
        builder.create(root)?;
        let lock = File::options()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(root.join("agent.lock"))?;
        lock.try_lock()
            .context("another local agent command uses this data directory")?;
        let jobs = root.join("signed-requests");
        builder.create(&jobs)?;
        Ok(Self {
            root: root.to_owned(),
            jobs,
            _lock: lock,
        })
    }

    pub(crate) fn wallet(&self, network: NetworkId, init: bool) -> Result<Wallet> {
        let profile_path = self.root.join("profile.json");
        let stored: Option<Profile> = match File::open(&profile_path) {
            Ok(file) => Some(serde_json::from_reader(file)?),
            Err(error) if init && error.kind() == ErrorKind::NotFound => None,
            Err(error) => return Err(error).context("run keryx-agent init first"),
        };
        let name = network.to_string();
        ensure!(
            stored
                .as_ref()
                .is_none_or(|profile| profile.network == name),
            "use a separate wallet directory for {name}"
        );
        let key_path = self.root.join("wallet-key.txt");
        if stored.is_none() && !key_path.try_exists()? {
            let mut key = Zeroizing::new([0u8; 32]);
            loop {
                OsRng.fill_bytes(key.as_mut());
                if escrow_pubkey_of(&key).is_ok() {
                    break;
                }
            }
            persist(
                &key_path,
                Zeroizing::new(hex::encode(key.as_ref())).as_bytes(),
            )?;
        }
        let key_text =
            Zeroizing::new(fs::read_to_string(key_path).context("load the local wallet key")?);
        let mut key = Zeroizing::new([0u8; 32]);
        hex::decode_to_slice(key_text.trim(), key.as_mut())?;
        let address = Address::new(network.into(), Version::PubKey, &escrow_pubkey_of(&key)?);
        let expected = Profile {
            network: name,
            address: address.to_string(),
        };
        ensure!(
            stored.as_ref().is_none_or(|saved| *saved == expected),
            "the key and wallet profile do not match"
        );
        if stored.is_none() {
            persist(&profile_path, &serde_json::to_vec(&expected)?)?;
        }
        Ok(Wallet { key, address })
    }

    pub(crate) fn save(&self, request: &LocalRequest) -> Result<()> {
        let id = request.transaction_id.to_string();
        let bytes = Zeroizing::new(serde_json::to_vec(request)?);
        persist(&self.jobs.join(format!("{id}.json")), &bytes)
    }

    pub(crate) fn get(&self, id: &str) -> Result<LocalRequest> {
        ensure!(valid_id(id), "expected a lowercase 64-character request ID");
        let file = File::open(self.jobs.join(format!("{id}.json")))
            .context("local request was not found")?;
        let prepared: LocalRequest = serde_json::from_reader(file)?;
        ensure!(
            prepared.transaction_id.to_string() == id,
            "local request ID does not match the file"
        );
        Ok(prepared)
    }

    pub(crate) fn reserved(&self, network: &str) -> Result<Outpoints> {
        let mut reserved = Outpoints::new();
        for entry in fs::read_dir(&self.jobs)? {
            let name = entry?.file_name();
            let Some(id) = name
                .to_str()
                .and_then(|name| name.strip_suffix(".json"))
                .filter(|id| valid_id(id))
            else {
                continue;
            };
            let prepared = self.get(id)?;
            if prepared.network == network {
                reserved.extend(prepared.transaction.inputs.iter().map(|i| {
                    (
                        i.previous_outpoint.transaction_id,
                        i.previous_outpoint.index,
                    )
                }));
            }
        }
        Ok(reserved)
    }

    pub(crate) fn prepared(&self, key: &str) -> Result<Option<LocalRequest>> {
        for entry in fs::read_dir(&self.jobs)? {
            let name = entry?.file_name();
            let Some(id) = name
                .to_str()
                .and_then(|name| name.strip_suffix(".json"))
                .filter(|id| valid_id(id))
            else {
                continue;
            };
            let saved = self.get(id)?;
            if saved.prepare_key.as_deref() == Some(key) {
                return Ok(Some(saved));
            }
        }
        Ok(None)
    }
}

fn valid_id(id: &str) -> bool {
    id.len() == 64
        && id
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn persist(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path.parent().context("missing wallet directory")?;
    let mut file = NamedTempFile::new_in(parent)?;
    file.write_all(bytes)?;
    file.as_file().sync_all()?;
    file.persist_noclobber(path)?;
    #[cfg(unix)]
    File::open(parent)?.sync_all()?;
    Ok(())
}
