// Command seqdex-initwallet drives the daemon's operator WalletService.InitWallet
// RPC (:9000), which creates the underlying ocean wallet (CreateWallet) and fires
// the daemon's onInit hook. Mnemonic is read from MNEMONIC_FILE and the password
// from PW_FILE so no secret material is on the command line.
//
// Throwaway regtest test helper.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	daemonv2 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/tdex-daemon/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := os.Getenv("OPERATOR_ADDR")
	if addr == "" {
		addr = "localhost:9000"
	}
	pwFile := os.Getenv("PW_FILE")
	mnFile := os.Getenv("MNEMONIC_FILE")
	if pwFile == "" || mnFile == "" {
		fmt.Fprintln(os.Stderr, "PW_FILE and MNEMONIC_FILE env required")
		os.Exit(1)
	}
	rawPw, err := os.ReadFile(pwFile)
	must(err, "read pw")
	rawMn, err := os.ReadFile(mnFile)
	must(err, "read mnemonic")
	password := string(bytes.TrimSpace(rawPw))
	mnemonic := strings.Fields(string(rawMn))
	if len(mnemonic) == 0 {
		fmt.Fprintln(os.Stderr, "empty mnemonic")
		os.Exit(1)
	}

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(err, "dial")
	defer conn.Close()

	cli := daemonv2.NewWalletServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := cli.InitWallet(ctx, &daemonv2.InitWalletRequest{
		Password:     password,
		SeedMnemonic: mnemonic,
		Restore:      false,
	})
	must(err, "InitWallet")

	for {
		reply, err := stream.Recv()
		if err == io.EOF {
			break
		}
		must(err, "InitWallet recv")
		if m := reply.GetMessage(); m != "" {
			fmt.Println("init msg:", m)
		}
	}
	fmt.Println("daemon InitWallet ok")
}

func must(err error, ctx string) {
	if err != nil {
		fmt.Fprintln(os.Stderr, ctx+":", err)
		os.Exit(1)
	}
}
