use std::{
    str::FromStr,
    time::{SystemTime, UNIX_EPOCH},
};

use base64::encode as base64_encode;
use jsonwebtoken::{encode as jwt_encode, Algorithm, EncodingKey, Header};
use pkcs1::EncodeRsaPrivateKey;
use pkcs8::der::Document;
use pkcs8::{DecodePrivateKey, EncodePublicKey};
use rsa::{RsaPrivateKey, RsaPublicKey};
use secrecy::{Secret, SecretString};
use serde::Serialize;
use sha2::{Digest, Sha256};
use tracing::info;

#[derive(Debug, Serialize)]
struct JwtClaims {
    iss: String,
    sub: String,
    iat: u64,
    exp: u64,
}

#[derive(Clone)]
pub struct SnowflakeAuth {
    account_id: String,
    normalized_account_id: String,
    username: String,
    private_key: RsaPrivateKey,
    public_key_fp: Option<String>,
    refresh_threshold: u64,
    expiry_threshold: u64,
    last_refreshed: u64,
    current_jwt: Option<Secret<String>>,
}

impl SnowflakeAuth {
    // When initializing, private_key must not be copied, to improve security of credentials.
    #[tracing::instrument(name = "peer_sflake::init_client_auth", skip_all)]
    pub fn new(
        account_id: String,
        username: String,
        private_key: String,
        refresh_threshold: u64,
        expiry_threshold: u64,
    ) -> Self {
        let mut snowflake_auth: SnowflakeAuth = SnowflakeAuth {
            // moved normalized_account_id above account_id to satisfy the borrow checker.
            normalized_account_id: SnowflakeAuth::normalize_account_identifier(&account_id),
            account_id,
            username,
            private_key: DecodePrivateKey::from_pkcs8_pem(&private_key).unwrap(),
            public_key_fp: None,
            refresh_threshold,
            expiry_threshold,
            last_refreshed: 0,
            current_jwt: None,
        };
        snowflake_auth.public_key_fp = Some(SnowflakeAuth::gen_public_key_fp(
            &snowflake_auth.private_key,
        ));
        snowflake_auth.refresh_jwt();
        snowflake_auth
    }

    // Normalize the account identifer to a form that is embedded into the JWT.
    // Logic adapted from Snowflake's example Python code for key-pair authentication "sql-api-generate-jwt.py".
    fn normalize_account_identifier(raw_account: &str) -> String {
        let split_index: usize;
        if !raw_account.contains(".global") {
            split_index = *raw_account
                .find(".")
                .get_or_insert(raw_account.chars().count());
        } else {
            split_index = *raw_account
                .find("-")
                .get_or_insert(raw_account.chars().count());
        }
        raw_account
            .to_uppercase()
            .chars()
            .take(split_index)
            .collect()
    }

    #[tracing::instrument(name = "peer_sflake::gen_public_key_fp", skip_all)]
    fn gen_public_key_fp(private_key: &RsaPrivateKey) -> String {
        let public_key =
            EncodePublicKey::to_public_key_der(&RsaPublicKey::from(private_key)).unwrap();
        format!(
            "SHA256:{}",
            base64_encode(Sha256::new_with_prefix(public_key.as_der()).finalize())
        )
    }

    #[tracing::instrument(name = "peer_sflake::auth_refresh_jwt", skip_all)]
    fn refresh_jwt(&mut self) {
        let private_key_jwt: EncodingKey = EncodingKey::from_rsa_der(
            EncodeRsaPrivateKey::to_pkcs1_der(&self.private_key)
                .unwrap()
                .as_der(),
        );
        self.last_refreshed = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs();
        info!(
            "Refreshing SnowFlake JWT for account: {} and user: {} at time {}",
            self.account_id, self.username, self.last_refreshed
        );
        let jwt_claims: JwtClaims = JwtClaims {
            iss: format!(
                "{}.{}.{}",
                self.normalized_account_id,
                self.username.to_uppercase(),
                self.public_key_fp.as_deref().unwrap()
            ),
            sub: format!(
                "{}.{}",
                self.normalized_account_id,
                self.username.to_uppercase()
            ),
            iat: self.last_refreshed,
            exp: self.last_refreshed + self.expiry_threshold,
        };
        let header: Header = Header::new(Algorithm::RS256);
        self.current_jwt = Some(
            SecretString::from_str(&jwt_encode(&header, &jwt_claims, &private_key_jwt).unwrap())
                .unwrap(),
        );
    }

    pub fn get_jwt(&mut self) -> &Secret<String> {
        if SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs()
            >= (self.last_refreshed + self.refresh_threshold)
        {
            self.refresh_jwt();
        }
        self.current_jwt.as_ref().unwrap()
    }
}