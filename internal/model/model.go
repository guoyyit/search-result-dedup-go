// Package model defines the core data types shared by the dedup service.
package model

// Document is a single indexed search result candidate.
type Document struct {
	// ID is the globally unique identifier of the document.
	ID string `json:"id"`
	// Source identifies the upstream origin (feed/site/tenant) of the item.
	Source string `json:"source"`
	// Title is a short headline of the document.
	Title string `json:"title,omitempty"`
	// Content is the full text used for near-duplicate comparison.
	Content string `json:"content"`
	// Score is the optional retrieval score assigned by the caller.
	// Higher means more relevant. It is never used by similarity itself.
	Score float64 `json:"score,omitempty"`
}
