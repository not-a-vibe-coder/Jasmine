// walmem-check verifies the Walrus Memory credentials in .env against the relayer
// and runs one remember/recall round trip.
//
//	go run ./cmd/walmem-check            # whoami + stats
//	go run ./cmd/walmem-check roundtrip  # also writes and recalls a test memory
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/joho/godotenv"

	"shipp/internal/walmem"
)

func main() {
	_ = godotenv.Load(".env")
	c, err := walmem.NewClient(os.Getenv("MEMWAL_SERVER_URL"), os.Getenv("MEMWAL_ACCOUNT_ID"), os.Getenv("MEMWAL_DELEGATE_KEY"))
	if err != nil {
		log.Fatalf("bad config: %v", err)
	}
	if c == nil {
		log.Fatal("MEMWAL_ACCOUNT_ID and MEMWAL_DELEGATE_KEY must be set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	who, err := c.Whoami(ctx)
	if err != nil {
		log.Fatalf("whoami failed: %v", err)
	}
	fmt.Printf("account: %s\nowner:   %s\npackage: %s\n", who.AccountID, who.Owner, who.PackageID)

	const ns = "jasmine-healthcheck"
	if st, err := c.Stats(ctx, ns); err == nil {
		fmt.Printf("stats(%s): %d memories, %d bytes\n", ns, st.MemoryCount, st.StorageBytes)
	} else {
		fmt.Printf("stats failed: %v\n", err)
	}

	if len(os.Args) < 2 || os.Args[1] != "roundtrip" {
		return
	}
	job, err := c.Remember(ctx, ns, "Jasmine healthcheck at "+time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		log.Fatalf("remember failed: %v", err)
	}
	st, err := c.WaitForBlob(ctx, job)
	if err != nil {
		log.Fatalf("remember job: %v", err)
	}
	fmt.Printf("stored blob %s\n%s\n", st.BlobID, walmem.BlobURL(st.BlobID))
	mems, err := c.Recall(ctx, ns, "Jasmine healthcheck", 3)
	if err != nil {
		log.Fatalf("recall failed: %v", err)
	}
	for _, m := range mems {
		fmt.Printf("recalled (%.3f): %s\n", m.Distance, m.Text)
	}
}
