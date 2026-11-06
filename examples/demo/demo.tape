# The output shown is replayed from a real run. To regenerate, from the repo root:
#   make build && examples/demo/capture.sh
#   docker run --rm -v "$PWD:/vhs" ghcr.io/charmbracelet/vhs examples/demo/demo.tape
Output examples/demo/demo.gif

Set Shell bash
Set FontSize 16
Set Width 1280
Set Height 420
Set Padding 16
Set TypingSpeed 35ms
Set Theme "Catppuccin Mocha"

Hide
Type "source examples/demo/replay.sh"
Enter
Sleep 500ms
Show

Type "aws --endpoint-url http://localhost:9000 s3 mb s3://site"
Enter
Sleep 2s
Type "aws --endpoint-url http://localhost:9000 s3 sync ./public s3://site"
Enter
Sleep 3s
Type "ls ./data/site"
Enter
Sleep 2s
Type "cat ./data/site/index.html"
Enter
Sleep 4s
