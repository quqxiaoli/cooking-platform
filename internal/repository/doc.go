// Package repository contains all data-access logic, wrapping GORM.
// Repositories are pure DB I/O — no business logic, no caching.
// Each method accepts a *gorm.DB to support external transaction passing.
package repository
