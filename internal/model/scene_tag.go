// Package model — scene_tag.go defines the 8 cooking-scene categories that
// anchor the platform's content taxonomy. Scene tags are stored as
// TINYINT UNSIGNED in MySQL (column posts.scene_tag), exposed as int8 in
// JSON, and represented by the SceneTag typed alias in Go.
//
// Why TINYINT + Go const, not MySQL ENUM:
//  1. Adding a tag = code change only; no ALTER TABLE on a million-row table.
//  2. Reordering ENUM values silently corrupts data; integers never change.
//  3. Portable across DB engines (PostgreSQL has its own enum type, SQLite has none).
//  4. Frontend can localise names without backend redeployment if we expose
//     i18n bundles client-side later.
//
// Future improvements:
//   - When secondary tags (taste / ingredient / diet) become a lookup table,
//     keep SceneTag here for the primary category axis only.
//   - i18n: Name() is hardcoded zh-CN today. Replace with a registry indexed
//     by locale once an English variant is needed.
//   - Add Parse(int8) (SceneTag, error) if anywhere outside DTO needs to
//     validate raw integers; current code uses int8 -> SceneTag(v).IsValid().
//
// Added in Step 4 (content module).
package model

// SceneTag is the typed alias for the eight canonical cooking scenes.
// Use SceneTag throughout the domain layer; convert to/from int8 only at
// the wire boundary (DTOs, JSON).
type SceneTag int8

// Scene tag values (matches PRD-Phase2 §9.1 ID column).
//
// Numeric values ARE the on-disk representation in posts.scene_tag.
// Rules for evolution:
//   - NEVER change an existing value's number — that silently rewrites
//     history for every existing post.
//   - NEVER reuse a deprecated number — pick the next free integer.
//   - To remove a scene: keep the constant, suffix it with `Deprecated`,
//     and stop offering it in CreatePostReq validation.
const (
	SceneTagUnknown  SceneTag = 0 // 0 = invalid placeholder; never written by code paths
	SceneTagRental   SceneTag = 1 // 出租屋
	SceneTagSolo     SceneTag = 2 // 一个人的饭
	SceneTagCamping  SceneTag = 3 // 露营野炊
	SceneTagFamily   SceneTag = 4 // 家庭厨房
	SceneTagQuick    SceneTag = 5 // 快手日常
	SceneTagBento    SceneTag = 6 // 打包便当
	SceneTagDiet     SceneTag = 7 // 减脂餐
	SceneTagSeasonal SceneTag = 8 // 节气节日
)

// sceneTagNames maps each defined tag to its display name. Map key order
// is irrelevant; canonical order lives in the const block above.
var sceneTagNames = map[SceneTag]string{
	SceneTagRental:   "出租屋",
	SceneTagSolo:     "一个人的饭",
	SceneTagCamping:  "露营野炊",
	SceneTagFamily:   "家庭厨房",
	SceneTagQuick:    "快手日常",
	SceneTagBento:    "打包便当",
	SceneTagDiet:     "减脂餐",
	SceneTagSeasonal: "节气节日",
}

// IsValid reports whether the tag is one of the 8 canonical scenes.
// SceneTagUnknown (0) and any out-of-range value return false.
func (s SceneTag) IsValid() bool {
	return s >= SceneTagRental && s <= SceneTagSeasonal
}

// Name returns the human-readable Chinese name, or empty string if invalid.
//
// Empty string is preferred over a placeholder like "未知" so callers can
// detect the bad-data case explicitly. This matters when serialising legacy
// rows that may have somehow been written with a now-deprecated tag value.
func (s SceneTag) Name() string {
	return sceneTagNames[s]
}
