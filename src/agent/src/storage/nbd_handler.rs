// Copyright (c) 2025 Red Hat
//
// SPDX-License-Identifier: Apache-2.0
//

use std::process::Command;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, Context, Result};
use kata_types::device::DRIVER_NBD_TYPE;
use kata_types::mount::StorageDevice;
use protocols::agent::Storage;
use tokio::time::sleep;
use tracing::instrument;

use crate::storage::{common_storage_handler, new_device, StorageContext, StorageHandler};

const NBD_DEVICE_WAIT_TIMEOUT_SECS: u64 = 10;

#[derive(Debug)]
pub struct NBDHandler {}

#[async_trait::async_trait]
impl StorageHandler for NBDHandler {
    #[instrument]
    fn driver_types(&self) -> &[&str] {
        &[DRIVER_NBD_TYPE]
    }

    #[instrument]
    async fn create_device(
        &self,
        storage: Storage,
        ctx: &mut StorageContext,
    ) -> Result<Arc<dyn StorageDevice>> {
        // Parse NBD URI from storage source (e.g., "nbd://server:port/export")
        let nbd_uri = &storage.source;
        if !nbd_uri.starts_with("nbd://") {
            return Err(anyhow!("Invalid NBD URI format: {}", nbd_uri));
        }

        // Extract server, port, and export name from URI
        let uri_parts = nbd_uri.strip_prefix("nbd://").unwrap();
        let (server_port, export_name) = match uri_parts.split_once('/') {
            Some((sp, export)) => (sp, export),
            None => return Err(anyhow!("NBD URI missing export name: {}", nbd_uri)),
        };

        let (server, port) = match server_port.split_once(':') {
            Some((s, p)) => (s, p),
            None => return Err(anyhow!("NBD URI missing port: {}", nbd_uri)),
        };

        info!(
            ctx.logger,
            "Connecting to NBD server";
            "server" => server,
            "port" => port,
            "export" => export_name,
        );

        // Find available NBD device
        let nbd_device = find_available_nbd_device().await?;

        // Connect using nbd-client
        let output = Command::new("nbd-client")
            .arg(server)
            .arg(port)
            .arg(&nbd_device)
            .arg("-name")
            .arg(export_name)
            .arg("-persist")
            .output()
            .context("Failed to execute nbd-client")?;

        if !output.status.success() {
            let stderr = String::from_utf8_lossy(&output.stderr);
            return Err(anyhow!("nbd-client failed: {}", stderr));
        }

        info!(
            ctx.logger,
            "Successfully connected NBD device";
            "device" => &nbd_device,
            "server" => server,
            "port" => port,
            "export" => export_name,
        );

        // Wait for device to be ready
        wait_for_nbd_device(&nbd_device).await?;

        // Use the NBD device path for common storage handling
        let mut nbd_storage = storage.clone();
        nbd_storage.source = nbd_device.clone();

        let path = common_storage_handler(ctx.logger, &nbd_storage)?;
        new_device(path)
    }
}

async fn find_available_nbd_device() -> Result<String> {
    // Check for available NBD devices (/dev/nbd0, /dev/nbd1, etc.)
    for i in 0..16 {
        let device_path = format!("/dev/nbd{}", i);

        // Check if device exists and is not in use
        if std::path::Path::new(&device_path).exists() {
            // Check if device is already connected by trying to get info
            let status = Command::new("nbd-client")
                .arg("-check")
                .arg(&device_path)
                .status();

            // If check fails, device is likely available
            if let Ok(status) = status {
                if !status.success() {
                    return Ok(device_path);
                }
            } else {
                return Ok(device_path);
            }
        }
    }

    Err(anyhow!("No available NBD devices found"))
}

async fn wait_for_nbd_device(device_path: &str) -> Result<()> {
    let start_time = std::time::Instant::now();
    let timeout = Duration::from_secs(NBD_DEVICE_WAIT_TIMEOUT_SECS);

    while start_time.elapsed() < timeout {
        if std::path::Path::new(device_path).exists() {
            // Check if device is readable (fully connected)
            if let Ok(_) = std::fs::metadata(device_path) {
                return Ok(());
            }
        }
        sleep(Duration::from_millis(100)).await;
    }

    Err(anyhow!("Timeout waiting for NBD device: {}", device_path))
}
