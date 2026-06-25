// Command seqdex-unlock drives the daemon's operator WalletService.UnlockWallet
// RPC, reading the password from the file named by PW_FILE so no secret is put
// on the command line. Unlocking through the daemon (rather than only through
// the ocean wallet) is what fires the daemon's onUnlock hook, which restarts
// the gRPC service WITH the trade interface (:9945).
//
// Throwaway regtest test helper.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	if pwFile == "" {
		fmt.Fprintln(os.Stderr, "PW_FILE env required")
		os.Exit(1)
	}
	raw, err := os.ReadFile(pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read pw:", err)
		os.Exit(1)
	}
	password := string(bytes.TrimSpace(raw))

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := daemonv2.NewWalletServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := client.UnlockWallet(ctx, &daemonv2.UnlockWalletRequest{
		Password: password,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "unlock:", err)
		os.Exit(1)
	}
	fmt.Println("daemon UnlockWallet ok")
}
