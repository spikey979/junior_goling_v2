package orchestrator

// NOTE: Ovo je skeleton za MuPDF selekciju stranica. U ovoj fazi ne koristimo
// stvarni MuPDF SDK; dodajemo strukture i minimalnu heuristiku za PoC.

type PageInfo struct {
    Page int
    HasImages bool
    TextDensity float64 // 0..1
}

type SelectionResult struct {
    MuPDFPages []int
    AIPages    []int
}

type SelectionOptions struct {
    TextOnly bool
    TotalPages int // ako nije poznato, koristi konservativni broj, npr. 3
}

// SelectPages implementira minimalnu heuristiku:
// - Privremeno: isključujemo MuPDF obradu i sve stranice šaljemo na AI.
func SelectPages(opts SelectionOptions) SelectionResult {
    if opts.TotalPages <= 0 { opts.TotalPages = 3 }
    res := SelectionResult{}
    for i := 1; i <= opts.TotalPages; i++ {
        res.AIPages = append(res.AIPages, i)
    }
    return res
}
