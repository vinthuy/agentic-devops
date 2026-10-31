package cli

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"agentdev/internal/webhook"
)

func webhookCmd(opts *options) *cobra.Command {
	addr := "127.0.0.1:8787"
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "接收 GitHub Webhook，只接入 Issue 或同步斜线命令",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			secret := strings.TrimSpace(os.Getenv("AGENTDEV_WEBHOOK_SECRET"))
			if secret == "" {
				return fail(ExitUsage, "需要环境变量 AGENTDEV_WEBHOOK_SECRET")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			handler := &webhook.Handler{
				Store:  st,
				Repos:  cfg.Repos,
				Secret: secret,
				Allow:  ghIssueClient{}.Collaborator,
				Poster: ghIssueClient{},
			}
			fmt.Fprintf(cmd.OutOrStdout(), "监听 %s\n", addr)
			if err := http.ListenAndServe(addr, handler); err != nil {
				return fail(ExitError, "Webhook 监听失败: %v", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", addr, "监听地址，默认只接受本机连接")
	return cmd
}
