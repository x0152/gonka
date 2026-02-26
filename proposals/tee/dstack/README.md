# dstack Deployment Notes (Minimal)

This folder contains the smallest compose to run vLLM inside a dstack CVM.

## File

- `docker-compose.vllm.yaml`

## Deploy flow

1. Ensure dstack control plane is running (VMM/KMS/Gateway).
2. Upload `docker-compose.vllm.yaml` via dstack VMM UI or CLI.
3. Wait until app status is healthy.
4. Capture endpoint URL and node metadata:
   - model name
   - node logical id
   - node public key exposed by your app layer
   - optional attestation payload/certificate
5. Register this node in chain:
   - submit `MsgSubmitNewParticipant` with:
     - `inference_url = https://<executor-dapi-url>`
     - `worker_key = <tee-node x25519 public key>`
   - submit `MsgSubmitHardwareDiff` for your TEE adapter endpoint:
     - `hardware.type = "TEE"`
     - `model = Qwen/Qwen2.5-7B-Instruct`
     - `host/port = <tee-node-endpoint>`

## Security notes

- This compose is intentionally minimal and uses `latest` tag for quick testing.
- For production, pin image by digest and add startup checks.
- Add response signing sidecar if you want verifiable usage receipts.
