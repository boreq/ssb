// SPDX-FileCopyrightText: 2021 The Go-SSB Authors
//
// SPDX-License-Identifier: MIT

package graph

import (
	"fmt"
	refs "go.mindeco.de/ssb-refs"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/encoding"
	"gonum.org/v1/gonum/graph/simple"
)

func (g *Graph) NodeCount() int {
	g.Mutex.Lock()
	defer g.Mutex.Unlock()
	return g.WeightedDirectedGraph.Nodes().Len()
}

type contactNode struct {
	graph.Node
	feed refs.FeedRef
	name string
}

func (n contactNode) String() string {
	if n.name != "" {
		return n.name
	}
	return n.feed.ShortSigil()
}

func (n contactNode) Attributes() []encoding.Attribute {
	name := fmt.Sprintf("%q", n.String())
	if n.name != "" {
		name = n.name
	}
	return []encoding.Attribute{
		{Key: "label", Value: name},
	}
}

type contactEdge struct {
	simple.WeightedEdge
	isBlock bool
}

func (n contactEdge) Attributes() []encoding.Attribute {
	c := "black"
	if n.W > 1 {
		c = "firebrick1"
	}
	return []encoding.Attribute{
		{Key: "color", Value: c},
		// {Key: "label", Value: fmt.Sprintf(`"%f"`, n.W)},
	}
}

type metafeedEdge struct {
	simple.WeightedEdge
}

func (n metafeedEdge) Attributes() []encoding.Attribute {
	c := "green"
	return []encoding.Attribute{
		{Key: "color", Value: c},
		// {Key: "label", Value: fmt.Sprintf(`"%f"`, n.W)},
	}
}
