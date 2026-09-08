' hidden.vbs — run a command with no console window (window style 0), wait for it, propagate its
' exit code. The scheduled-task action for a vLLM seat, so the engine's wsl.exe never flashes a
' window in the operator's interactive session.
Dim sh: Set sh = CreateObject("WScript.Shell")
WScript.Quit sh.Run(WScript.Arguments(0), 0, True)
