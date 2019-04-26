/*
 * Copyright (c) 2019. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

package endpoint

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/micro/go-micro/errors"

	"github.com/pydio/cells/common"
	"github.com/pydio/cells/common/log"
	natsbroker "github.com/pydio/cells/common/micro/broker/nats"
	natsregistry "github.com/pydio/cells/common/micro/registry/nats"
	grpctransport "github.com/pydio/cells/common/micro/transport/grpc"
	"github.com/pydio/cells/common/proto/tree"
	"github.com/pydio/cells/common/registry"
	"github.com/pydio/cells/common/sync/model"
	"github.com/pydio/cells/common/views"
)

func init() {
	natsregistry.Enable()
	natsbroker.Enable()
	grpctransport.Enable()

}

type NoopWriter struct{}

func (nw *NoopWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (nw *NoopWriter) Close() error {
	return nil
}

type RouterEndpoint struct {
	router    *views.Router
	root      string
	ctx       context.Context
	options   Options
	watchConn chan model.WatchConnectionInfo
}

type Options struct {
	RenewFolderUuids bool
}

func NewRouterEndpoint(root string, options Options) *RouterEndpoint {
	return &RouterEndpoint{
		root:    root,
		options: options,
	}
}

func (r *RouterEndpoint) LoadNode(ctx context.Context, path string, leaf ...bool) (node *tree.Node, err error) {
	resp, e := r.getRouter().ReadNode(r.getContext(ctx), &tree.ReadNodeRequest{Node: &tree.Node{Path: r.rooted(path)}})
	if e != nil {
		return nil, e
	}
	out := resp.Node
	out.Path = r.unrooted(resp.Node.Path)
	return out, nil
}

func (r *RouterEndpoint) GetEndpointInfo() model.EndpointInfo {
	return model.EndpointInfo{
		URI: "router://" + r.root,
		RequiresNormalization: false,
		RequiresFoldersRescan: false,
		EchoTime:              30 * time.Second,
		Ignores:               []string{common.PYDIO_SYNC_HIDDEN_FILE_META},
	}
}

func (r *RouterEndpoint) Walk(walknFc model.WalkNodesFunc, pathes ...string) (err error) {
	p := ""
	if len(pathes) > 0 {
		p = pathes[0]
	}
	log.Logger(r.getContext()).Debug("Walking Router on " + r.rooted(p))
	s, e := r.getRouter().ListNodes(r.getContext(), &tree.ListNodesRequest{
		Node:      &tree.Node{Path: r.rooted(p)},
		Recursive: true,
	})
	if e != nil {
		return e
	}
	defer s.Close()
	for {
		resp, e := s.Recv()
		if e != nil {
			break
		}
		n := resp.Node
		if n.Etag == common.NODE_FLAG_ETAG_TEMPORARY /*|| path.Base(n.Path) == common.PYDIO_SYNC_HIDDEN_FILE_META*/ {
			continue
		}
		n.Path = r.unrooted(resp.Node.Path)
		if !n.IsLeaf() {
			n.Etag = "-1" // Force recomputing Etags for Folders
		}
		walknFc(n.Path, n, nil)
	}
	return
}

func (r *RouterEndpoint) Watch(recursivePath string, connectionInfo chan model.WatchConnectionInfo) (*model.WatchObject, error) {

	r.watchConn = connectionInfo
	changes := make(chan *tree.NodeChangeEvent)
	finished := make(chan error)
	ctx, cancel := context.WithCancel(r.getContext())

	obj := &model.WatchObject{
		EventInfoChan: make(chan model.EventInfo),
		DoneChan:      make(chan bool, 1),
		ErrorChan:     make(chan error),
	}
	go func() {
		defer close(finished)
		defer close(obj.EventInfoChan)
		for {
			select {
			case c := <-changes:
				r.changeToEventInfo(obj.EventInfoChan, c)
			case er := <-finished:
				log.Logger(r.getContext()).Info("Connection finished " + er.Error())
				if connectionInfo != nil {
					connectionInfo <- model.WatchDisconnected
				}
				<-time.After(5 * time.Second)
				log.Logger(r.getContext()).Info("Restarting events watcher after 5s")
				go r.receiveEvents(ctx, changes, finished)
			case <-obj.DoneChan:
				log.Logger(r.getContext()).Info("Stopping event watcher")
				cancel()
				return
			}
		}
	}()

	go r.receiveEvents(ctx, changes, finished)

	return obj, nil
}

func (r *RouterEndpoint) changeValidPath(n *tree.Node) bool {
	if n == nil {
		return true
	}
	if n.Etag == common.NODE_FLAG_ETAG_TEMPORARY {
		return false
	}
	if strings.Trim(n.Path, "/") == "" {
		return false
	}
	if path.Base(n.Path) == common.PYDIO_SYNC_HIDDEN_FILE_META {
		return false
	}
	return true
}

func (r *RouterEndpoint) changeToEventInfo(events chan model.EventInfo, change *tree.NodeChangeEvent) {

	TimeFormatFS := "2006-01-02T15:04:05.000Z"
	now := time.Now().UTC().Format(TimeFormatFS)
	if !r.changeValidPath(change.Target) || !r.changeValidPath(change.Source) {
		return
	}

	if change.Type == tree.NodeChangeEvent_CREATE || change.Type == tree.NodeChangeEvent_UPDATE_CONTENT {
		log.Logger(r.getContext()).Debug("Got Event " + change.Type.String() + " - " + change.Target.Path)
		events <- model.EventInfo{
			Type:           model.EventCreate,
			Path:           change.Target.Path,
			Etag:           change.Target.Etag,
			Time:           now,
			Folder:         !change.Target.IsLeaf(),
			Size:           change.Target.Size,
			PathSyncSource: r,
		}
	} else if change.Type == tree.NodeChangeEvent_DELETE {
		log.Logger(r.getContext()).Debug("Got Event " + change.Type.String() + " - " + change.Source.Path)
		events <- model.EventInfo{
			Type:           model.EventRemove,
			Path:           change.Source.Path,
			Time:           now,
			PathSyncSource: r,
		}
	} else if change.Type == tree.NodeChangeEvent_UPDATE_PATH {
		log.Logger(r.getContext()).Debug("Got Move Event " + change.Type.String() + " - " + change.Source.Path + " - " + change.Target.Path)
		events <- model.EventInfo{
			Type:           model.EventSureMove,
			Path:           change.Target.Path,
			Folder:         !change.Target.IsLeaf(),
			Size:           change.Target.Size,
			Etag:           change.Target.Etag,
			MoveSource:     change.Source,
			MoveTarget:     change.Target,
			PathSyncSource: r,
		}
	}
	return
}

func (r *RouterEndpoint) receiveEvents(ctx context.Context, changes chan *tree.NodeChangeEvent, finished chan error) {
	changesClient := tree.NewNodeChangesStreamerClient(registry.GetClient(common.SERVICE_TREE))
	streamer, e := changesClient.StreamChanges(r.getContext(), &tree.StreamChangesRequest{RootPath: r.root})
	if e != nil {
		finished <- e
		return
	}
	if r.watchConn != nil {
		r.watchConn <- model.WatchConnected
	}
	for {
		change, e := streamer.Recv()
		if e != nil {
			log.Logger(r.getContext()).Error("Stopping watcher on error" + e.Error())
			finished <- e
			break
		}
		changes <- change
	}
}

func (r *RouterEndpoint) ComputeChecksum(node *tree.Node) error {
	return fmt.Errorf("not.implemented")
}

func (r *RouterEndpoint) CreateNode(ctx context.Context, node *tree.Node, updateIfExists bool) (err error) {
	n := node.Clone()
	n.Path = r.rooted(n.Path)
	if r.options.RenewFolderUuids {
		n.Uuid = ""
	}
	_, e := r.getRouter().CreateNode(r.getContext(ctx), &tree.CreateNodeRequest{Node: n})
	return e
}

func (r *RouterEndpoint) UpdateNode(ctx context.Context, node *tree.Node) (err error) {
	n := node.Clone()
	n.Path = r.rooted(n.Path)
	_, e := r.getRouter().CreateNode(r.getContext(ctx), &tree.CreateNodeRequest{Node: n})
	return e
}

func (r *RouterEndpoint) DeleteNode(ctx context.Context, name string) (err error) {
	// Ignore .pydio files !
	if path.Base(name) == common.PYDIO_SYNC_HIDDEN_FILE_META {
		log.Logger(ctx).Debug("[router] Ignoring " + name)
		return nil
	}
	router := r.getRouter()
	ctx = r.getContext(ctx)
	read, e := router.ReadNode(ctx, &tree.ReadNodeRequest{Node: &tree.Node{Path: r.rooted(name)}})
	if e != nil {
		if errors.Parse(e.Error()).Code == 404 {
			return nil
		} else {
			return e
		}
	}
	_, err = router.DeleteNode(ctx, &tree.DeleteNodeRequest{Node: read.Node.Clone()})
	return
}

// MoveNode renames a file or folder and *blocks* until the node has been properly moved (sync)
func (r *RouterEndpoint) MoveNode(ctx context.Context, oldPath string, newPath string) (err error) {
	if from, err := r.LoadNode(ctx, oldPath); err == nil {
		to := from.Clone()
		to.Path = r.rooted(newPath)
		from.Path = r.rooted(from.Path)
		_, e := r.getRouter().UpdateNode(r.getContext(ctx), &tree.UpdateNodeRequest{From: from, To: to})
		if e == nil {
			// Block until move is correctly indexed
			model.Retry(func() error {
				_, e := r.getRouter().ReadNode(r.getContext(ctx), &tree.ReadNodeRequest{Node: to})
				return e
			}, 1*time.Second, 10*time.Second)
		}
		return e
	} else {
		return err
	}
}

func (r *RouterEndpoint) GetWriterOn(p string, targetSize int64) (out io.WriteCloser, err error) {
	if targetSize == 0 {
		return nil, fmt.Errorf("cannot create empty files")
	}
	if path.Base(p) == common.PYDIO_SYNC_HIDDEN_FILE_META {
		log.Logger(r.getContext()).Debug("[router] Ignoring " + p)
		return &NoopWriter{}, nil
	}
	n := &tree.Node{Path: r.rooted(p)}
	reader, out := io.Pipe()
	go func() {
		_, e := r.getRouter().PutObject(r.getContext(), n, reader, &views.PutRequestData{Size: targetSize})
		if e != nil {
			fmt.Println("[ERROR]", "Cannot PutObject", e.Error())
		}
		reader.Close()
	}()
	return out, nil

}

func (r *RouterEndpoint) GetReaderOn(p string) (out io.ReadCloser, err error) {
	n := &tree.Node{Path: r.rooted(p)}
	o, e := r.getRouter().GetObject(r.getContext(), n, &views.GetRequestData{StartOffset: 0, Length: -1})
	return o, e
}

func (r *RouterEndpoint) getRouter() *views.Router {
	if r.router == nil {
		r.router = views.NewStandardRouter(views.RouterOptions{
			WatchRegistry:    true,
			AdminView:        true,
			SynchronousTasks: true,
		})
	}
	return r.router
}

func (r *RouterEndpoint) getContext(ctx ...context.Context) context.Context {
	c := context.Background()
	if len(ctx) > 0 {
		c = ctx[0]
	}
	return context.WithValue(c, common.PYDIO_CONTEXT_USER_KEY, common.PYDIO_SYSTEM_USERNAME)
}

func (r *RouterEndpoint) rooted(p string) string {
	return path.Join(r.root, p)
}

func (r *RouterEndpoint) unrooted(p string) string {
	return strings.TrimLeft(strings.TrimPrefix(p, r.root), "/")
}
