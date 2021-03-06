// Copyright (c) 2018 Ashley Jeffs
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package processor

import (
	"fmt"

	"github.com/Jeffail/benthos/lib/log"
	"github.com/Jeffail/benthos/lib/message"
	"github.com/Jeffail/benthos/lib/metrics"
	"github.com/Jeffail/benthos/lib/processor/condition"
	"github.com/Jeffail/benthos/lib/response"
	"github.com/Jeffail/benthos/lib/types"
)

//------------------------------------------------------------------------------

func init() {
	Constructors[TypeGroupBy] = TypeSpec{
		constructor: NewGroupBy,
		description: `
Splits a batch of messages into N batches, where each resulting batch contains a
group of messages determined by conditions that are applied per message of the
original batch. Once the groups are established a list of processors are applied
to their respective grouped batch, which can be used to label the batch as per
their grouping.

Each group is configured in a list with a condition and a list of processors:

` + "``` yaml" + `
type: group_by
group_by:
  - condition:
      type: static
      static: true
    processors:
      - type: noop
` + "```" + `

Messages are added to the first group that passes and can only belong to a
single group. Messages that do not pass the conditions of any group are placed
in a final batch with no processors applied.

For example, imagine we have a batch of messages that we wish to split into two
groups - the foos and the bars - which should be sent to different output
destinations based on those groupings. We also need to send the foos as a tar
gzip archive. For this purpose we can use the ` + "`group_by`" + ` processor
with a ` + "[`switch`](../outputs/README.md#switch)" + ` output:

` + "``` yaml" + `
pipeline:
  processors:
  - type: group_by
    group_by:
    - condition:
        type: text
        text:
          operator: contains
          arg: "this is a foo"
      processors:
      - type: archive
        archive:
          format: tar
      - type: compress
        compress:
          algorithm: gzip
      - type: metadata
        metadata:
          operator: set
          key: grouping
          value: foo
output:
  type: switch
  switch:
    outputs:
    - output:
        type: foo_output
      condition:
        type: metadata
        metadata:
          operator: equals
          key: grouping
          arg: foo
    - output:
        type: bar_output
` + "```" + `

Since any message that isn't a foo is a bar, and bars do not require their own
processing steps, we only need a single grouping configuration.`,
		sanitiseConfigFunc: func(conf Config) (interface{}, error) {
			groups := []interface{}{}
			for _, g := range conf.GroupBy {
				condSanit, err := condition.SanitiseConfig(g.Condition)
				if err != nil {
					return nil, err
				}
				procsSanit := []interface{}{}
				for _, p := range g.Processors {
					var procSanit interface{}
					if procSanit, err = SanitiseConfig(p); err != nil {
						return nil, err
					}
					procsSanit = append(procsSanit, procSanit)
				}
				groups = append(groups, map[string]interface{}{
					"condition":  condSanit,
					"processors": procsSanit,
				})
			}
			return groups, nil
		},
	}
}

//------------------------------------------------------------------------------

// GroupByElement represents a group determined by a condition and a list of
// group specific processors.
type GroupByElement struct {
	Condition  condition.Config `json:"condition" yaml:"condition"`
	Processors []Config         `json:"processors" yaml:"processors"`
}

//------------------------------------------------------------------------------

// GroupByConfig is a configuration struct containing fields for the GroupBy
// processor, which breaks message batches down into N batches of a smaller size
// according to conditions.
type GroupByConfig []GroupByElement

// NewGroupByConfig returns a GroupByConfig with default values.
func NewGroupByConfig() GroupByConfig {
	return GroupByConfig{}
}

//------------------------------------------------------------------------------

type group struct {
	Condition  condition.Type
	Processors []Type
}

// GroupBy is a processor that group_bys messages into a message per part.
type GroupBy struct {
	log   log.Modular
	stats metrics.Type

	groups     []group
	mGroupPass []metrics.StatCounter

	mGroupDefault metrics.StatCounter
	mCount        metrics.StatCounter
	mDropped      metrics.StatCounter
	mSent         metrics.StatCounter
	mSentParts    metrics.StatCounter
}

// NewGroupBy returns a GroupBy processor.
func NewGroupBy(
	conf Config, mgr types.Manager, log log.Modular, stats metrics.Type,
) (Type, error) {
	var err error
	groups := make([]group, len(conf.GroupBy))
	groupCtrs := make([]metrics.StatCounter, len(conf.GroupBy))

	for i, gConf := range conf.GroupBy {
		groupPrefix := fmt.Sprintf("processor.group_by.groups.%v", i)
		nsLog := log.NewModule(groupPrefix)
		nsStats := metrics.Namespaced(stats, groupPrefix)

		if groups[i].Condition, err = condition.New(gConf.Condition, mgr, nsLog, nsStats); err != nil {
			return nil, fmt.Errorf("failed to create condition for group '%v': %v", i, err)
		}
		for j, pConf := range gConf.Processors {
			var proc Type
			if proc, err = New(pConf, mgr, nsLog, nsStats); err != nil {
				return nil, fmt.Errorf("failed to create processor '%v' for group '%v': %v", j, i, err)
			}
			groups[i].Processors = append(groups[i].Processors, proc)
		}

		groupCtrs[i] = stats.GetCounter(groupPrefix + ".passed")
	}

	return &GroupBy{
		log:   log.NewModule(".processor.group_by"),
		stats: stats,

		groups:     groups,
		mGroupPass: groupCtrs,

		mGroupDefault: stats.GetCounter("processor.group_by.groups.default.passed"),
		mCount:        stats.GetCounter("processor.group_by.count"),
		mDropped:      stats.GetCounter("processor.group_by.dropped"),
		mSent:         stats.GetCounter("processor.group_by.sent"),
		mSentParts:    stats.GetCounter("processor.group_by.parts.sent"),
	}, nil
}

//------------------------------------------------------------------------------

// ProcessMessage applies the processor to a message, either creating >0
// resulting messages or a response to be sent back to the message source.
func (g *GroupBy) ProcessMessage(msg types.Message) ([]types.Message, types.Response) {
	g.mCount.Incr(1)

	if msg.Len() == 0 {
		g.mDropped.Incr(1)
		return nil, response.NewAck()
	}

	groups := make([]types.Message, len(g.groups))
	for i := range groups {
		groups[i] = message.New(nil)
	}
	groupless := message.New(nil)

	msg.Iter(func(i int, p types.Part) error {
		for j, group := range g.groups {
			if group.Condition.Check(message.Lock(msg, i)) {
				groups[j].Append(p.Copy())
				g.mGroupPass[j].Incr(1)
				return nil
			}
		}

		groupless.Append(p.Copy())
		g.mGroupDefault.Incr(1)
		return nil
	})

	msgs := []types.Message{}
	for i, gmsg := range groups {
		if gmsg.Len() == 0 {
			continue
		}

		resultMsgs := []types.Message{gmsg}
		var res types.Response
		for j := 0; len(resultMsgs) > 0 && j < len(g.groups[i].Processors); j++ {
			var nextResultMsgs []types.Message
			for _, m := range resultMsgs {
				var rMsgs []types.Message
				rMsgs, res = g.groups[i].Processors[j].ProcessMessage(m)
				nextResultMsgs = append(nextResultMsgs, rMsgs...)
			}
			resultMsgs = nextResultMsgs
		}

		if len(resultMsgs) > 0 {
			msgs = append(msgs, resultMsgs...)
		}
		if res != nil {
			if err := res.Error(); err != nil {
				g.log.Errorf("Processor error: %v\n", err)
			}
		}
	}

	if groupless.Len() > 0 {
		msgs = append(msgs, groupless)
	}

	if len(msgs) == 0 {
		g.mDropped.Incr(1)
		return nil, response.NewAck()
	}

	g.mSent.Incr(int64(len(msgs)))
	for _, m := range msgs {
		g.mSentParts.Incr(int64(m.Len()))
	}
	return msgs, nil
}

//------------------------------------------------------------------------------
