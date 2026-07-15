@echo off
REM Double-click to launch the local-model coding agent GUI.
REM Starts the agent server + OpenWebUI (in WSL) then opens the browser.
echo Starting local agent stack (this can take ~15s the first time)...
wsl.exe -e bash -lc "bash /mnt/d/repos/local-offload/scripts/openwebui-stack.sh"
start "" "http://localhost:8081"
