package common_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/relay"
)

// The local realm's procedures must stay outside every prefix xconn bridges to remote
// clients, or remote users could call them.
func TestLocalProceduresAreNotBridged(t *testing.T) {
	local := []string{
		common.ProcedureProxyFileOp, common.ProcedureProxyMachineInfo, common.ProcedureProxyPing,
		common.ProcedureProxyCat, common.ProcedureProxyPrinterList, common.ProcedureProxyPrinterPrint,
		common.ProcedureProxyVPNStart, common.ProcedureProxyVPNStop, common.ProcedureLogin,
		common.ProcedureLogout, common.ProcedureConnect, common.ProcedureDisconnect,
		common.ProcedureDisconnectAll, common.ProcedureConnectedMachines, common.ProcedureAppsRegister,
	}
	bridged := append([]string{common.ProcedurePrefix, common.ProcedureAdminPrefix}, relay.AppPrefixes()...)
	for _, procedure := range local {
		require.True(t, strings.HasPrefix(procedure, common.LocalProcedurePrefix), procedure)
		for _, prefix := range bridged {
			require.False(t, strings.HasPrefix(procedure, prefix), "%s is under %s", procedure, prefix)
		}
	}
}
