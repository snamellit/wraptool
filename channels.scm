;; Channel lock for evaluating manifest.scm from a clean checkout.
(use-modules (guix channels))

(list
 (channel
  (name 'snamellit)
  (url "https://forge.snamellit.com/pti/snamguix.git")
  (commit "6ac31c06260313fbb270f086fbdcb8393bc40ef0")
  (introduction
   (make-channel-introduction
    "8e0f7748eeeb89cbfa3d9c1274ee555b7af868dd"
    (openpgp-fingerprint
     "C8AB C91C AB2D C195 B976 3877 DFAB C43C 55F2 E63A"))))
 (channel
  (name 'nonguix)
  (url "https://gitlab.com/nonguix/nonguix.git")
  (commit "3b66965566fe8c96edb5a41fd39a9e5a90ad9b61")
  (introduction
   (make-channel-introduction
    "897c1a470da759236cc11798f4e0a5f7d4d59fbc"
    (openpgp-fingerprint
     "2A39 3FFF 68F4 EF7A 3D29 12AF 6F51 20A0 22FB B2D5"))))
 (channel
  (name 'guix)
  (url "https://git.savannah.gnu.org/git/guix.git")
  (commit "36d403cfd77ff5452978cf94425902675e6ad81b")
  (introduction
   (make-channel-introduction
    "9edb3f66fd807b096b48283debdcddccfea34bad"
    (openpgp-fingerprint
     "BBB0 2DDF 2CEA F6A8 0D1D E643 A2A0 6DF2 A33A 54FA")))))
