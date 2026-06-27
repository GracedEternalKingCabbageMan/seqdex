// Command seqob-octl is a tiny operator helper for inspecting and preparing
// Ocean wallet accounts when setting up a SeqOB order-book maker. It reuses the
// daemon's Ocean client; it is read-mostly (balance/address) with an explicit
// create action. No keys, no signing.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aejkcs50/seqdex/daemon/internal/core/application"
	oceanwallet "github.com/aejkcs50/seqdex/daemon/internal/infrastructure/ocean-wallet"
)

func main() {
	ocean := flag.String("ocean", "localhost:19500", "ocean wallet endpoint")
	account := flag.String("account", "", "Ocean account name")
	action := flag.String("action", "balance", "balance | address | create (create then print an address)")
	num := flag.Int("n", 1, "number of addresses to derive (address action)")
	flag.Parse()
	if *account == "" {
		fatal("-account is required")
	}

	w, err := oceanwallet.NewService(*ocean)
	if err != nil {
		fatal("connect ocean %q: %v", *ocean, err)
	}
	svc, err := application.NewWalletService(w, "")
	if err != nil {
		fatal("wallet service: %v", err)
	}
	defer svc.Close()
	ctx := context.Background()
	acct := svc.Account()

	switch *action {
	case "create":
		info, err := acct.CreateAccount(ctx, *account, false)
		if err != nil {
			fatal("create account %q: %v", *account, err)
		}
		fmt.Printf("created account %q: %+v\n", *account, info)
		fallthrough
	case "address":
		addrs, err := acct.DeriveAddresses(ctx, *account, *num)
		if err != nil {
			fatal("derive addresses for %q: %v", *account, err)
		}
		for _, a := range addrs {
			fmt.Printf("address[%s] %s\n", *account, a)
		}
	case "balance":
		bal, err := acct.GetBalance(ctx, *account)
		if err != nil {
			fatal("balance for %q: %v", *account, err)
		}
		if len(bal) == 0 {
			fmt.Printf("balance[%s]: (empty)\n", *account)
		}
		for asset, b := range bal {
			fmt.Printf("balance[%s] %s confirmed=%d unconfirmed=%d locked=%d total=%d\n",
				*account, asset, b.GetConfirmedBalance(), b.GetUnconfirmedBalance(),
				b.GetLockedBalance(), b.GetTotalBalance())
		}
	default:
		fatal("unknown -action %q (balance|address|create)", *action)
	}
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}
