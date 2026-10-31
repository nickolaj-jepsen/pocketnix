package dev.nickolaj.pocketnix.managed

import java.io.File
import java.util.zip.GZIPOutputStream
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

class PackageIndexTest {
    @get:Rule val tmp = TemporaryFolder()

    private val packages = listOf(
        Package("ripgrep-all", "0.10", "Ripgrep, but also search in PDFs, E-Books, Office documents"),
        Package("python3Packages.ripgrepy", "2.0", "Python interface to ripgrep"),
        Package("ripgrep", "15.0", "Utility that combines the usability of The Silver Searcher with the raw speed of grep"),
        Package("fd", "10.2", "Simple, fast and user-friendly alternative to find"),
        Package("gnome-calculator", "48", "Application that solves mathematical equations")
    )

    @Test
    fun readsTheReleasesIndex() {
        val f = File(tmp.root, "index.tsv.gz")
        GZIPOutputStream(f.outputStream()).bufferedWriter().use { w ->
            w.write("ripgrep\t15.0\tUtility that combines\tsearch\n")
            w.write("fd\t10.2\t\n")
            w.write("\n")
        }
        assertEquals(
            listOf(Package("ripgrep", "15.0", "Utility that combines\tsearch"), Package("fd", "10.2", "")),
            PackageIndex.parse(f)
        )
    }

    @Test
    fun theClosestNameComesFirst() {
        val got = PackageIndex.search(packages, "ripgrep").map { it.attr }
        assertEquals("ripgrep", got.first())
        // A package set's attribute after the top-level ones.
        assertEquals("python3Packages.ripgrepy", got.last())
    }

    @Test
    fun everyWordMatchesNameOrDescription() {
        assertEquals(listOf("gnome-calculator"), PackageIndex.search(packages, "gnome equations").map { it.attr })
        assertEquals(listOf("fd"), PackageIndex.search(packages, "find").map { it.attr })
        assertTrue(PackageIndex.search(packages, "  ").isEmpty())
    }
}
