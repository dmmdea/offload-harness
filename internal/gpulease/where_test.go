package gpulease

import "testing"

// Info.Where is the filter For is built on (plan P7): a consumer that asks about only some
// of the live leases (the ones that refuse new work, the ones on a seat's cards) narrows
// the inspection, then asks the predicates it always asked of the result.

func TestWhereKeepsOnlyTheAcceptedLeasesInTheShapeForReturns(t *testing.T) {
	a, b, c := mediaOn(3, idCard0), mediaOn(5, idCard1), mediaOn(8, idCard2)
	b.Class = ClassText
	all := a
	all.Leases = []Info{a, b, c}
	all.Epochs = []uint64{3, 5, 8}

	text := all.Where(func(l Info) bool { return l.Class == ClassText })
	if !text.Held || text.Epoch != 5 || len(text.Leases) != 0 || len(text.Epochs) != 1 || text.Epochs[0] != 5 {
		t.Fatalf("one accepted lease must come back as the Info itself: %+v", text)
	}

	media := all.Where(func(l Info) bool { return l.Class == ClassMedia })
	if !media.Held || media.Epoch != 3 || len(media.Leases) != 2 || media.Leases[1].Epoch != 8 {
		t.Fatalf("several accepted leases are the lowest epoch with Leases carrying each: %+v", media)
	}
	if len(media.Epochs) != 2 || media.Epochs[0] != 3 || media.Epochs[1] != 8 {
		t.Fatalf("Epochs must list exactly the accepted leases: %v", media.Epochs)
	}

	if none := all.Where(func(Info) bool { return false }); none.Held {
		t.Fatalf("nothing accepted is the zero Info: %+v", none)
	}
	if (Info{}).Where(func(Info) bool { return true }).Held {
		t.Fatal("an Info that holds nothing has nothing to keep")
	}
}

func TestForIsWhereTouches(t *testing.T) {
	a, c := mediaOn(3, idCard0), mediaOn(8, idCard2)
	all := a
	all.Leases = []Info{a, c}
	all.Epochs = []uint64{3, 8}
	got := all.For([]string{idCard2})
	if !got.Held || got.Epoch != 8 || len(got.Leases) != 0 {
		t.Fatalf("For(card 2) = %+v, want the card-2 lease alone", got)
	}
	if unchanged := all.For(nil); len(unchanged.Leases) != 2 {
		t.Fatalf("an unknown card set is every card and returns the inspection unchanged: %+v", unchanged)
	}
}
