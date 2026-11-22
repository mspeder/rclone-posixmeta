package main

import (
	_ "github.com/rclone/rclone/backend/all"
	_ "github.com/rclone/rclone/cmd/all"
	_ "github.com/mspeder/rclone-posixmeta/backend/posixmeta"
	"github.com/rclone/rclone/cmd"
)

func main() {
	cmd.Main()
}
