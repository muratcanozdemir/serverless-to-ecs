package main

import (
	"os"

	"serverless-to-ecs/cmd"
)

func main() {
	os.Exit(cmd.Run())
}
