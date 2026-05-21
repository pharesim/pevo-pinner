package main

import (
	"fmt"
	"path/filepath"

	bsblockstore "github.com/ipfs/boxo/blockstore"
	ds "github.com/ipfs/go-datastore"
	flatfs "github.com/ipfs/go-ds-flatfs"
)

// openRepo opens (or creates) the flatfs datastore at <repoDir>/blocks and
// wraps it in a boxo blockstore. flatfs is the Kubo default: sharded one-
// file-per-block layout, inspectable with `ls`, easy backups, no compaction
// surprises. The sharding func is the same NextToLast(2) Kubo writes by
// default, so legacy `ipfs add` repos and ours are layout-compatible.
func openRepo(repoDir string) (ds.Batching, bsblockstore.Blockstore, error) {
	blocksDir := filepath.Join(repoDir, "blocks")
	shardFn, err := flatfs.ParseShardFunc("/repo/flatfs/shard/v1/next-to-last/2")
	if err != nil {
		return nil, nil, fmt.Errorf("parsing shard func: %w", err)
	}
	dstore, err := flatfs.CreateOrOpen(blocksDir, shardFn, false)
	if err != nil {
		return nil, nil, fmt.Errorf("opening flatfs datastore: %w", err)
	}
	bs := bsblockstore.NewBlockstore(dstore)
	return dstore, bs, nil
}
