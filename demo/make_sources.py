"""Write the demo source documents (fictional content, MIT like the rest).

    python3 demo/make_sources.py demo/src

Word then exports each to PDF (demo/run.sh): the PDFs a reader is sent.
"""

import sys
from pathlib import Path

from docx import Document
from docx.enum.table import WD_TABLE_ALIGNMENT
from docx.enum.text import WD_ALIGN_PARAGRAPH, WD_TAB_ALIGNMENT, WD_TAB_LEADER
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.shared import Pt, RGBColor, Inches


def base(font, size):
    d = Document()
    st = d.styles["Normal"]
    st.font.name, st.font.size = font, Pt(size)
    st.paragraph_format.space_after = Pt(6)
    for s in d.sections:
        s.left_margin = s.right_margin = Inches(1)
    return d


def shade(cell, fill):
    pr = cell._tc.get_or_add_tcPr()
    sh = OxmlElement("w:shd")
    sh.set(qn("w:val"), "clear")
    sh.set(qn("w:fill"), fill)
    pr.append(sh)


def page_field(paragraph):
    run = paragraph.add_run()
    for kind, text in (("begin", None), (None, "PAGE"), ("end", None)):
        if kind:
            el = OxmlElement("w:fldChar")
            el.set(qn("w:fldCharType"), kind)
        else:
            el = OxmlElement("w:instrText")
            el.text = text
        run._r.append(el)


CLAUSES = [
    ("Definitions", [
        "“Services” means the calibration, repair and inspection work described in each Work Order, together with any related reports, certificates and advice the Supplier provides to the Customer under this Agreement.",
        "“Work Order” means a written order for Services that references this Agreement and is signed by an authorised representative of each party.",
    ]),
    ("Term and Renewal", [
        "This Agreement begins on the Effective Date and continues for an initial term of thirty-six (36) months. It renews automatically for successive periods of twelve (12) months unless either party gives written notice of non-renewal at least ninety (90) days before the end of the then-current term.",
    ]),
    ("Fees and Payment", [
        "The Customer shall pay the fees set out in each Work Order within forty-five (45) days of receiving a correct invoice. Fees are exclusive of value added tax, which the Customer shall pay at the rate in force on the invoice date.",
        "If an invoice is disputed in good faith, the Customer shall pay the undisputed portion on time and notify the Supplier of the disputed amount and the reasons within fifteen (15) days of receipt.",
    ]),
    ("Service Levels", [
        "The Supplier shall respond to a priority-one fault within four (4) business hours and restore the affected instrument to service within two (2) business days. Service credits for missed targets are set out in Schedule 2 and are the Customer’s sole financial remedy for that failure.",
    ]),
    ("Limitation of Liability", [
        "Neither party excludes or limits liability for death or personal injury caused by its negligence, for fraud, or for any liability that cannot be limited by law.",
        "Subject to clause 5.1, each party’s total liability arising out of or in connection with this Agreement in any contract year shall not exceed one hundred and fifty per cent (150%) of the fees paid or payable in that year.",
    ]),
]


def contract(out):
    d = base("Times New Roman", 11)
    t = d.add_paragraph()
    t.alignment = WD_ALIGN_PARAGRAPH.CENTER
    r = t.add_run("MASTER SERVICES AGREEMENT")
    r.bold, r.font.size = True, Pt(16)
    p = d.add_paragraph("between Halden Instruments Ltd (the “Supplier”) and Brightwater Analytics LLC (the “Customer”)")
    p.alignment = WD_ALIGN_PARAGRAPH.CENTER
    h = d.add_paragraph()
    h.add_run("Contents").bold = True
    for i, (title, _) in enumerate(CLAUSES, 1):
        p = d.add_paragraph(f"{i}.\t{title}\t{i + 1}")
        p.paragraph_format.tab_stops.add_tab_stop(Inches(0.4))
        p.paragraph_format.tab_stops.add_tab_stop(Inches(6.5), WD_TAB_ALIGNMENT.RIGHT, WD_TAB_LEADER.DOTS)
        p.paragraph_format.space_after = Pt(2)
    d.add_paragraph()
    for i, (title, paras) in enumerate(CLAUSES, 1):
        p = d.add_paragraph()
        p.add_run(f"{i}.  {title.upper()}").bold = True
        p.paragraph_format.space_before = Pt(10)
        for j, text in enumerate(paras, 1):
            p = d.add_paragraph(f"{i}.{j}\t{text}")
            p.alignment = WD_ALIGN_PARAGRAPH.JUSTIFY
            p.paragraph_format.left_indent = Inches(0.5)
            p.paragraph_format.first_line_indent = Inches(-0.5)
    d.add_paragraph()
    sig = d.add_table(rows=4, cols=2)
    for row, (a, b) in zip(sig.rows, [("Signed for the Supplier", "Signed for the Customer"), ("", ""), ("Name: ____________________", "Name: ____________________"), ("Date: ____________________", "Date: ____________________")]):
        row.cells[0].text, row.cells[1].text = a, b
    page_field(d.sections[0].footer.paragraphs[0])
    d.sections[0].footer.paragraphs[0].alignment = WD_ALIGN_PARAGRAPH.CENTER
    d.save(out)


def report(out):
    d = base("Calibri", 11)
    hdr = d.sections[0].header.paragraphs[0]
    hdr.text = "Brightwater Analytics — Quarterly Operations Report"
    hdr.runs[0].font.size, hdr.runs[0].font.color.rgb = Pt(9), RGBColor(0x59, 0x59, 0x59)
    d.add_heading("Q3 Operations Report", 0)
    d.add_heading("Summary", 1)
    d.add_paragraph(
        "Laboratory throughput rose 14% on the previous quarter, driven by the second sample-preparation line that came online in July. "
        "Turnaround time fell below the five-day target for the first time, and instrument downtime halved after the calibration contract moved to a priority-one response.")
    for b in ["Samples processed: 18,420 (Q2: 16,150)", "Median turnaround: 4.6 days (target 5.0)", "Instrument availability: 98.7%", "Two audit findings closed; none open"]:
        d.add_paragraph(b, style="List Bullet")
    d.add_heading("Throughput by site", 1)
    rows = [("Site", "Q2", "Q3", "Change"), ("Leeds", "6,210", "7,040", "+13.4%"), ("Rotterdam", "5,480", "6,330", "+15.5%"), ("Porto", "4,460", "5,050", "+13.2%"), ("Total", "16,150", "18,420", "+14.1%")]
    t = d.add_table(rows=len(rows), cols=4)
    t.style = "Table Grid"
    t.alignment = WD_TABLE_ALIGNMENT.CENTER
    for i, vals in enumerate(rows):
        for j, v in enumerate(vals):
            c = t.cell(i, j)
            c.text = v
            para = c.paragraphs[0]
            para.paragraph_format.space_after = Pt(0)
            if j:
                para.alignment = WD_ALIGN_PARAGRAPH.RIGHT
            if i in (0, len(rows) - 1):
                para.runs[0].bold = True
            if i == 0:
                shade(c, "1F3864")
                para.runs[0].font.color.rgb = RGBColor(0xFF, 0xFF, 0xFF)
    d.add_heading("Outlook", 1)
    d.add_paragraph(
        "The fourth quarter will add weekend shifts in Rotterdam and retire two legacy chromatography systems. "
        "We expect throughput to reach 19,500 samples with turnaround held under five days; the main risk is reagent supply, which is covered by a second qualified vendor from October.")
    page_field(d.sections[0].footer.paragraphs[0])
    d.sections[0].footer.paragraphs[0].alignment = WD_ALIGN_PARAGRAPH.RIGHT
    d.save(out)


def invoice(out):
    d = base("Arial", 10)
    t = d.add_paragraph()
    r = t.add_run("Halden Instruments Ltd")
    r.bold, r.font.size, r.font.color.rgb = True, Pt(18), RGBColor(0x1F, 0x38, 0x64)
    d.add_paragraph("Unit 4, Riverside Park · Leeds LS10 1AB · VAT GB 000 0000 00")
    p = d.add_paragraph()
    p.add_run("INVOICE").bold = True
    p.runs[0].font.size = Pt(14)
    meta = d.add_table(rows=3, cols=4)
    for row, vals in zip(meta.rows, [("Invoice no.", "HI-2026-0417", "Bill to", "Brightwater Analytics LLC"), ("Date", "3 October 2026", "", "Attn: Accounts Payable"), ("Due", "17 November 2026", "", "PO 55-1893")]):
        for c, v in zip(row.cells, vals):
            c.text = v
            c.paragraphs[0].paragraph_format.space_after = Pt(0)
        row.cells[0].paragraphs[0].runs[0].bold = True
        if row.cells[2].text:
            row.cells[2].paragraphs[0].runs[0].bold = True
    d.add_paragraph()
    lines = [("Description", "Qty", "Unit price", "Amount"),
             ("Annual calibration, HPLC system", "6", "£1,250.00", "£7,500.00"),
             ("Priority-one call-out (WO 2291)", "2", "£480.00", "£960.00"),
             ("Replacement detector lamp", "3", "£315.00", "£945.00"),
             ("Inspection certificates", "9", "£40.00", "£360.00")]
    t = d.add_table(rows=len(lines) + 3, cols=4)
    t.style = "Table Grid"
    for i, vals in enumerate(lines):
        for j, v in enumerate(vals):
            c = t.cell(i, j)
            c.text = v
            c.paragraphs[0].paragraph_format.space_after = Pt(0)
            if j:
                c.paragraphs[0].alignment = WD_ALIGN_PARAGRAPH.RIGHT
            if i == 0:
                c.paragraphs[0].runs[0].bold = True
                shade(c, "D9E2F3")
    for k, (label, amount) in enumerate([("Subtotal", "£9,765.00"), ("VAT at 20%", "£1,953.00"), ("Total due", "£11,718.00")]):
        row = t.rows[len(lines) + k]
        a = row.cells[0].merge(row.cells[2])
        a.text, row.cells[3].text = label, amount
        for c in (a, row.cells[3]):
            c.paragraphs[0].alignment = WD_ALIGN_PARAGRAPH.RIGHT
            c.paragraphs[0].paragraph_format.space_after = Pt(0)
            if k == 2:
                c.paragraphs[0].runs[0].bold = True
    d.add_paragraph()
    d.add_paragraph("Payment by bank transfer to sort code 00-00-00, account 00000000, quoting the invoice number. Late payments accrue interest at 4% above base rate.")
    d.save(out)


if __name__ == "__main__":
    out = Path(sys.argv[1])
    out.mkdir(parents=True, exist_ok=True)
    contract(out / "contract.docx")
    report(out / "report.docx")
    invoice(out / "invoice.docx")
