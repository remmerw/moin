package moin

import (
	"context"
	"fmt"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-merkledag"
	ipfspath "github.com/ipfs/go-path"
	"github.com/ipfs/go-path/resolver"
	ft "github.com/ipfs/go-unixfs"
	uio "github.com/ipfs/go-unixfs/io"
	"github.com/ipfs/interface-go-ipfs-core/path"
	gopath "path"
)

const (
	// TFile is a regular file.
	TFile = 2
	// TDirectory is a directory.
	TDirectory = 1
	// TSymlink is a symlink.
	TSymlink = 4
)

// DirEntry is a directory entry returned by `Ls`.
type DirEntry struct {
	Name string
	Cid  cid.Cid

	// Only filled when asked to resolve the directory entry.
	Size uint64 // The size of the file in bytes (or the size of the symlink).
	Type int32  // The type of the file.

	Err error
}

func (n *Node) ResolveNode(ctx context.Context, dag ipld.DAGService, p path.Path) (ipld.Node, error) {
	rp, err := n.ResolvePath(ctx, dag, p)
	if err != nil {
		return nil, err
	}

	node, err := dag.Get(ctx, rp.Cid())
	if err != nil {
		return nil, err
	}
	return node, nil
}

// ResolvePath resolves the path `p` using Unixfs resolver, returns the
// resolved path.
func (n *Node) ResolvePath(ctx context.Context, dag ipld.DAGService, p path.Path) (path.Resolved, error) {
	if _, ok := p.(path.Resolved); ok {
		return p.(path.Resolved), nil
	}
	if err := p.IsValid(); err != nil {
		return nil, err
	}

	ipath := ipfspath.Path(p.String())

	var resolveOnce resolver.ResolveOnce

	switch ipath.Segments()[0] {
	case "ipfs":
		resolveOnce = uio.ResolveUnixfsOnce
	case "ipld":
		resolveOnce = resolver.ResolveSingle
	default:
		return nil, fmt.Errorf("unsupported path namespace: %s", p.Namespace())
	}

	r := &resolver.Resolver{
		DAG:         dag,
		ResolveOnce: resolveOnce,
	}

	node, rest, err := r.ResolveToLastNode(ctx, ipath)
	if err != nil {
		return nil, err
	}

	root, err := cid.Parse(ipath.Segments()[1])
	if err != nil {
		return nil, err
	}

	return path.NewResolvedPath(ipath, node, root, gopath.Join(rest...)), nil
}

func (n *Node) lsFromLinksAsync(ctx context.Context, dag ipld.DAGService,
	dir uio.Directory, resolveChildren bool) (<-chan DirEntry, error) {

	out := make(chan DirEntry)

	go func() {
		defer close(out)
		for l := range dir.EnumLinksAsync(ctx) {
			select {
			case out <- n.processLink(ctx, dag, l, resolveChildren): //TODO: perf: processing can be done in background and in parallel
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func (n *Node) processLink(ctx context.Context, dag ipld.DAGService, linkres ft.LinkResult, resolveChildren bool) DirEntry {
	lnk := DirEntry{
		Name: linkres.Link.Name,
		Cid:  linkres.Link.Cid,
		Err:  linkres.Err,
	}
	if lnk.Err != nil {
		return lnk
	}

	switch lnk.Cid.Type() {
	case cid.Raw:
		// No need to check with raw leaves
		lnk.Type = TFile
		lnk.Size = linkres.Link.Size
	case cid.DagProtobuf:
		if !resolveChildren {
			break
		}

		linkNode, err := linkres.Link.GetNode(ctx, dag)
		if err != nil {
			lnk.Err = err
			break
		}

		if pn, ok := linkNode.(*merkledag.ProtoNode); ok {
			d, err := ft.FSNodeFromBytes(pn.Data())
			if err != nil {
				lnk.Err = err
				break
			}
			switch d.Type() {
			case ft.TFile, ft.TRaw:
				lnk.Type = TFile
			case ft.THAMTShard, ft.TDirectory, ft.TMetadata:
				lnk.Type = TDirectory
			case ft.TSymlink:
				lnk.Type = TSymlink
			}
			lnk.Size = d.FileSize()
		}
	}

	return lnk
}

func (n *Node) lsFromLinks(ctx context.Context, dag ipld.DAGService, ndlinks []*ipld.Link, resolveChildren bool) (<-chan DirEntry, error) {
	links := make(chan DirEntry, len(ndlinks))
	for _, l := range ndlinks {
		lr := ft.LinkResult{Link: &ipld.Link{Name: l.Name, Size: l.Size, Cid: l.Cid}}

		links <- n.processLink(ctx, dag, lr, resolveChildren) //TODO: can be parallel if settings.Async
	}
	close(links)
	return links, nil
}

func (n *Node) CidCheck(multihash string) error {

	var err error
	_, err = cid.Decode(multihash)
	if err != nil {
		return fmt.Errorf("invalid content id")
	}
	return nil
}
