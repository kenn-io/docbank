package mcp

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/answercard"
	"go.kenn.io/docbank/internal/daemonconn"
)

var answerCardsTool = toolDefinition{
	name: "match_answer_cards", title: "Match answer cards",
	description: "Match a question to owner-approved answer cards, checking current sources and exact quotations live. On no_match use search_documents. Card files found by raw search are not evidence.",
	schemas:     matchAnswerCardsSchemas,
}

type matchAnswerCardsOutput struct {
	answercard.MatchResult
	privateCache
}

func matchAnswerCards(ctx context.Context, lease *daemonLease, raw []byte) (matchAnswerCardsOutput, error) {
	var input struct {
		Question string `json:"question"`
		Root     string `json:"root"`
		Limit    int    `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return matchAnswerCardsOutput{}, err
	}
	lease.mu.Lock()
	if lease.answerCards == nil {
		lease.answerCards = answercard.NewMatcher()
	}
	matcher := lease.answerCards
	lease.mu.Unlock()
	var limitErr error
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (answercard.MatchResult, error) {
		result, err := matcher.Match(ctx, c, input.Question, input.Root, input.Limit)
		if errors.Is(err, answercard.ErrCardLimit) {
			limitErr = err
			return result, nil
		}
		return result, err
	})
	if err != nil {
		return matchAnswerCardsOutput{}, err
	}
	if limitErr != nil {
		return matchAnswerCardsOutput{}, limitErr
	}
	return matchAnswerCardsOutput{MatchResult: result, privateCache: newPrivateCache()}, nil
}
