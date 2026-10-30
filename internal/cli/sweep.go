package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"agentdev/internal/orchestrator"
)

func sweepCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "sweep",
		Short: "处理已到期的审批和锁，不启动任务",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			result, err := orchestrator.ExpirePending(st, cfg)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if result.ExpiredApprovals == 0 && result.ExpiredLocks == 0 {
				fmt.Fprintln(out, "没有到期项")
				return nil
			}
			if result.ExpiredApprovals > 0 {
				fmt.Fprintf(out, "审批超时：%d 个任务已升级为 NEEDS_HUMAN（可用 task resume 送回审批门）\n", result.ExpiredApprovals)
			}
			fmt.Fprintf(out, "已处理 %d 项（审批 %d，锁 %d），跳过 %d 项\n",
				result.ExpiredApprovals+result.ExpiredLocks,
				result.ExpiredApprovals, result.ExpiredLocks, result.Skipped)
			return nil
		},
	}
}
