package taxonomy

import (
	"context"
	"testing"
)

// fakeResolver — справочник жанров в виде обычной map, без единого похода
// в базу. Ключи — уже нормализованные теги (нижний регистр, без пробелов),
// значения — id жанра, который якобы нашёлся.
type fakeResolver map[string]int32

func (f fakeResolver) ResolveAlias(_ context.Context, aliasKey string) (int32, bool, error) {
	id, ok := f[aliasKey]
	return id, ok, nil
}

func TestResolve_Basic(t *testing.T) {
	resolver := fakeResolver{
		"rock": 1,
		"jazz": 2,
	}

	matches, unmapped, err := Resolve(context.Background(), resolver, "lastfm", []Tag{
		{Name: "rock", Weight: 100},
		{Name: "seen live", Weight: 50}, // не в справочнике
	})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	if len(unmapped) != 1 || unmapped[0] != "seen live" {
		t.Errorf("ожидали unmapped=[\"seen live\"], получили: %+v", unmapped)
	}

	if len(matches) != 1 {
		t.Fatalf("ожидали 1 совпадение, получили %d: %+v", len(matches), matches)
	}
	// lastfm вес 0.35, сила тега 100/100 = 1.0 → уверенность 0.35.
	if matches[0].GenreID != 1 || matches[0].Confidence != 0.35 {
		t.Errorf("неверный расчёт: %+v", matches[0])
	}
}

// TestResolve_DuplicateGenreTakesMax проверяет ключевой случай: два тега
// ("punk" и "punk rock") ведут в один и тот же жанр — уверенность не должна
// удвоиться, должен остаться максимум из двух.
func TestResolve_DuplicateGenreTakesMax(t *testing.T) {
	resolver := fakeResolver{
		"punk":      7,
		"punk rock": 7,
	}

	matches, _, err := Resolve(context.Background(), resolver, "lastfm", []Tag{
		{Name: "punk", Weight: 40},      // сила 0.4 → уверенность 0.14
		{Name: "punk rock", Weight: 90}, // сила 0.9 → уверенность 0.315
	})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("ожидали ровно одно совпадение (один жанр), получили %d: %+v", len(matches), matches)
	}
	if matches[0].GenreID != 7 {
		t.Fatalf("не тот жанр: %+v", matches[0])
	}
	// Должен остаться максимум (0.315 от "punk rock"), а не сумма (0.455).
	want := 0.35 * 0.9
	if diff := matches[0].Confidence - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ожидали %.4f, получили %.4f", want, matches[0].Confidence)
	}
}

func TestResolve_CaseAndWhitespaceNormalized(t *testing.T) {
	resolver := fakeResolver{"rock": 1}

	matches, unmapped, err := Resolve(context.Background(), resolver, "lastfm", []Tag{
		{Name: "  ROCK  ", Weight: 100},
	})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(unmapped) != 0 {
		t.Errorf("ожидали ноль unmapped, получили: %+v", unmapped)
	}
	if len(matches) != 1 || matches[0].GenreID != 1 {
		t.Errorf("тег в другом регистре с пробелами не нашёлся: %+v", matches)
	}
}

func TestResolve_UnknownProvider(t *testing.T) {
	_, _, err := Resolve(context.Background(), fakeResolver{}, "unknown-provider", nil)
	if err == nil {
		t.Fatal("ожидали ошибку про неизвестный провайдер, получили nil")
	}
}

func TestResolve_EmptyTagNameSkipped(t *testing.T) {
	resolver := fakeResolver{"rock": 1}
	matches, unmapped, err := Resolve(context.Background(), resolver, "lastfm", []Tag{
		{Name: "   ", Weight: 50},
	})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(matches) != 0 || len(unmapped) != 0 {
		t.Errorf("пустой тег должен просто игнорироваться, получили matches=%+v unmapped=%+v", matches, unmapped)
	}
}
