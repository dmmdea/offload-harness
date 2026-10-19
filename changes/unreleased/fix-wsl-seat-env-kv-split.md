### Changed - the rendered WSL2 seat env exports HOUSE_KV_SPLIT=1

- **What.** `setup/templates/vllm-seat/windows-wsl/seat.env` now carries `export HOUSE_KV_SPLIT=1`, with a comment that says what it does. The vLLM patches README and the operator guide no longer tell the operator to add the line after rendering.
- **Why.** A WSL2 seat on vLLM 0.29 or later dies at start unless the house KV-split patch is applied and this variable is set. The renderer did not write it, so every rendered seat needed a hand edit. An unpatched vLLM never reads the variable, so the line is safe on any version.
- **Check.** The rendered template contains the export, and `seat_fg.sh` sources the env file, so the variable reaches vLLM the same way `VLLM_EXECUTE_MODEL_TIMEOUT_SECONDS` does. The production pair seat has run with the same line since 2026-10-18.
