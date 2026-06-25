// Command genseedhelper calls the running Ocean wallet's GenSeed gRPC and
// writes the returned 24-word mnemonic to the file given by OUT_FILE.
//
// This is a throwaway test helper (regtest) used to obtain a fresh mnemonic
// without putting seed material on a shell command line.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	pb "github.com/aejkcs50/seqdex/wallet/api-spec/protobuf/gen/go/ocean/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := os.Getenv("WALLET_ADDR")
	if addr == "" {
		addr = "localhost:18000"
	}
	outFile := os.Getenv("OUT_FILE")
	if outFile == "" {
		fmt.Fprintln(os.Stderr, "OUT_FILE env required")
		os.Exit(1)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := pb.NewWalletServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.GenSeed(ctx, &pb.GenSeedRequest{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "genseed:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outFile, []byte(resp.GetMnemonic()), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote mnemonic to", outFile)
}
