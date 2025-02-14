package dagcmd

import (
	"errors"
	"io"

	cid "github.com/ipfs/go-cid"
	cmds "github.com/ipfs/go-ipfs-cmds"
	ipld "github.com/ipfs/go-ipld-format"
	ipldlegacy "github.com/ipfs/go-ipld-legacy"
	"github.com/ipfs/go-libipfs/files"
	"github.com/ipfs/interface-go-ipfs-core/options"
	gocarv2 "github.com/ipld/go-car/v2"

	"github.com/ipfs/kubo/core/commands/cmdenv"
	"github.com/ipfs/kubo/core/commands/cmdutils"
)

func dagImport(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
	node, err := cmdenv.GetNode(env)
	if err != nil {
		return err
	}

	api, err := cmdenv.GetApi(env, req)
	if err != nil {
		return err
	}

	// on import ensure we do not reach out to the network for any reason
	// if a pin based on what is imported + what is in the blockstore
	// isn't possible: tough luck
	api, err = api.WithOptions(options.Api.Offline(true))
	if err != nil {
		return err
	}

	doPinRoots, _ := req.Options[pinRootsOptionName].(bool)

	// grab a pinlock ( which doubles as a GC lock ) so that regardless of the
	// size of the streamed-in cars nothing will disappear on us before we had
	// a chance to roots that may show up at the very end
	// This is especially important for use cases like dagger:
	//    ipfs dag import $( ... | ipfs-dagger --stdout=carfifos )
	//
	if doPinRoots {
		unlocker := node.Blockstore.PinLock(req.Context)
		defer unlocker.Unlock(req.Context)
	}

	// this is *not* a transaction
	// it is simply a way to relieve pressure on the blockstore
	// similar to pinner.Pin/pinner.Flush
	batch := ipld.NewBatch(req.Context, api.Dag())

	roots := cid.NewSet()
	var blockCount, blockBytesCount uint64

	it := req.Files.Entries()
	for it.Next() {
		file := files.FileFromEntry(it)
		if file == nil {
			return errors.New("expected a file handle")
		}

		// import blocks
		err = func() error {
			// wrap a defer-closer-scope
			//
			// every single file in it() is already open before we start
			// just close here sooner rather than later for neatness
			// and to surface potential errors writing on closed fifos
			// this won't/can't help with not running out of handles
			defer file.Close()

			car, err := gocarv2.NewBlockReader(file)
			if err != nil {
				return err
			}

			for _, c := range car.Roots {
				roots.Add(c)
			}

			for {
				block, err := car.Next()
				if err != nil && err != io.EOF {
					return err
				} else if block == nil {
					break
				}
				if err := cmdutils.CheckBlockSize(req, uint64(len(block.RawData()))); err != nil {
					return err
				}

				// the double-decode is suboptimal, but we need it for batching
				nd, err := ipldlegacy.DecodeNode(req.Context, block)
				if err != nil {
					return err
				}

				if err := batch.Add(req.Context, nd); err != nil {
					return err
				}
				blockCount++
				blockBytesCount += uint64(len(block.RawData()))
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	if err := batch.Commit(); err != nil {
		return err
	}

	// It is not guaranteed that a root in a header is actually present in the same ( or any )
	// .car file. This is the case in version 1, and ideally in further versions too.
	// Accumulate any root CID seen in a header, and supplement its actual node if/when encountered
	// We will attempt a pin *only* at the end in case all car files were well-formed.

	// opportunistic pinning: try whatever sticks
	if doPinRoots {
		err = roots.ForEach(func(c cid.Cid) error {
			ret := RootMeta{Cid: c}

			// This will trigger a full read of the DAG in the pinner, to make sure we have all blocks.
			// Ideally we would do colloring of the pinning state while importing the blocks
			// and ensure the gray bucket is empty at the end (or use the network to download missing blocks).
			if block, err := node.Blockstore.Get(req.Context, c); err != nil {
				ret.PinErrorMsg = err.Error()
			} else if nd, err := ipldlegacy.DecodeNode(req.Context, block); err != nil {
				ret.PinErrorMsg = err.Error()
			} else if err := node.Pinning.Pin(req.Context, nd, true); err != nil {
				ret.PinErrorMsg = err.Error()
			} else if err := node.Pinning.Flush(req.Context); err != nil {
				ret.PinErrorMsg = err.Error()
			}

			return res.Emit(&CarImportOutput{Root: &ret})
		})
		if err != nil {
			return err
		}
	}

	stats, _ := req.Options[statsOptionName].(bool)
	if stats {
		err = res.Emit(&CarImportOutput{
			Stats: &CarImportStats{
				BlockCount:      blockCount,
				BlockBytesCount: blockBytesCount,
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}
