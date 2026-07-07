package roundtable

import "testing"

func TestValidateTranslationProviderResultRequiresQuestionTitle(t *testing.T) {
	err := validateTranslationProviderResult(translatableResource{
		Type:  "question",
		Title: "Question title",
	}, TranslationProviderResult{
		Title: " ",
		Body:  "Translated body",
	})
	if err == nil {
		t.Fatal("expected question translation with empty title to be rejected")
	}

	if err := validateTranslationProviderResult(translatableResource{
		Type:  "answer",
		Title: "",
	}, TranslationProviderResult{
		Title: "",
		Body:  "Translated body",
	}); err != nil {
		t.Fatalf("answer translation with empty title rejected: %v", err)
	}
}
