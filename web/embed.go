package webui

import "embed"

// Assets are built into the executable so a privileged service never runs
// JavaScript supplied by its working directory.
//
//go:embed index.html styles.css app.js favicon.svg
var Assets embed.FS
